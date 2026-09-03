package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	updateManifestSchemaVersion = 2
	updateTrustSchemaVersion    = 1
	stableUpdateChannel         = "stable"
	testingUpdateChannel        = "testing"
	updateHealthTimeout         = 2 * time.Minute
	maxUpdateReleaseFeedBytes   = 4 * 1024 * 1024
)

type updateTrust struct {
	SchemaVersion  int    `json:"schemaVersion"`
	Channel        string `json:"channel"`
	ManifestURL    string `json:"manifestUrl,omitempty"`
	ReleaseFeedURL string `json:"releaseFeedUrl,omitempty"`
	PublicKey      string `json:"publicKey"`
}

type updateTransaction struct {
	Target          string    `json:"target"`
	Previous        string    `json:"previous"`
	ExpectedVersion string    `json:"expectedVersion"`
	ConfigPath      string    `json:"configPath"`
	ActivatedAt     time.Time `json:"activatedAt"`
	Deadline        time.Time `json:"deadline"`
}

type updateManifest struct {
	SchemaVersion  int                       `json:"schemaVersion"`
	Channel        string                    `json:"channel"`
	Version        string                    `json:"version"`
	ReleaseTag     string                    `json:"releaseTag"`
	SourceRevision string                    `json:"sourceRevision"`
	Artifacts      map[string]updateArtifact `json:"artifacts"`
}

type releaseFeedItem struct {
	TagName    string             `json:"tag_name"`
	Draft      bool               `json:"draft"`
	Prerelease bool               `json:"prerelease"`
	Assets     []releaseFeedAsset `json:"assets"`
}

type releaseFeedAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type updateArtifact struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`
}

type updateRelease struct {
	Manifest updateManifest
	Platform string
	Artifact updateArtifact
}

var (
	approvedUpdateExecutable = os.Executable
	startApprovedUpdate      = launchStagedUpdate
	isManagedUpdateConfig    = func(path string) bool { return path == defaultConfigPath() }
	waitForUpdateParent      = waitForProcessExit
	updateServiceAction      = serviceAction
	validateApprovedUpdate   = validateInstalledUpdate
	waitForApprovedUpdate    = waitForUpdatedRuntimeReady
)

func updateRelay(ctx context.Context, cfg *config, configPath string, args []string, client *http.Client) error {
	if distribution != "native" {
		return errors.New("telrad update is available only for native installations")
	}
	if len(args) > 1 {
		return errors.New("usage: telrad update [exact-version]")
	}
	trust, err := loadUpdateTrust(cfg, configPath)
	if err != nil {
		return err
	}
	release, err := fetchUpdateRelease(ctx, trust, client)
	if err != nil {
		return err
	}
	precedence, err := compareReleaseVersions(version, release.Manifest.Version)
	if err != nil {
		return err
	}
	printUpdateRelease(release, precedence)
	if len(args) == 0 {
		fmt.Println("No changes made.")
		if precedence < 0 {
			fmt.Println("Inspect the source for this exact release, then approve it with:")
			if runtime.GOOS == "windows" {
				fmt.Printf("  telrad update %s\n", release.Manifest.Version)
			} else {
				fmt.Printf("  sudo telrad update %s\n", release.Manifest.Version)
			}
		}
		return nil
	}
	approvedVersion := args[0]
	if !isManagedUpdateConfig(configPath) {
		return errors.New("applying updates requires the managed native installation")
	}
	if _, err := parseReleaseVersion(approvedVersion); err != nil {
		return fmt.Errorf("approved update version: %w", err)
	}
	if approvedVersion != release.Manifest.Version {
		return fmt.Errorf("approved version %s is not the currently signed release %s; no changes made", approvedVersion, release.Manifest.Version)
	}
	if precedence == 0 {
		return fmt.Errorf("Relay %s is already installed; no changes made", version)
	}
	if precedence > 0 {
		return fmt.Errorf("refusing to downgrade Relay from %s to %s", version, approvedVersion)
	}
	if err := checkRuntimeReady(configPath, time.Now()); err != nil {
		return fmt.Errorf("Relay must be running and ready before an update: %w", err)
	}
	runtimeStatus, err := readRuntimeStatus(configPath)
	if err != nil {
		return err
	}
	if runtimeStatus.Version != version {
		return fmt.Errorf("running Relay version %s does not match this update command version %s", runtimeStatus.Version, version)
	}
	binary, err := download(ctx, client, release.Artifact.URL, 100*1024*1024)
	if err != nil {
		return err
	}
	publicKey, signature, err := decodeUpdateSignature(trust.PublicKey, release.Artifact.Signature)
	if err != nil {
		return err
	}
	if err := verifyUpdateArtifact(publicKey, release.Manifest, release.Platform, release.Artifact, signature, binary); err != nil {
		return err
	}
	current, err := approvedUpdateExecutable()
	if err != nil {
		return err
	}
	staged := current + ".new"
	if err := atomicWriteFile(staged, binary, 0o755); err != nil {
		return fmt.Errorf("stage approved update: %w", err)
	}
	fmt.Println("Artifact signature and SHA-256: verified")
	fmt.Printf("Applying clinic-approved Relay %s...\n", approvedVersion)
	if runtime.GOOS == "windows" {
		fmt.Println("Keep this Administrator terminal open until the update reports completion.")
	}
	if err := startApprovedUpdate(staged, []string{
		"apply-update",
		"-target", current,
		"-version", approvedVersion,
		"-config", configPath,
	}); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("launch approved update: %w", err)
	}
	return nil
}

func loadUpdateTrust(cfg *config, configPath string) (updateTrust, error) {
	var trust updateTrust
	if configPath == defaultConfigPath() {
		if err := validateManagedUpdateTrust(defaultUpdateTrustPath()); err != nil {
			return trust, err
		}
		data, err := os.ReadFile(defaultUpdateTrustPath())
		if errors.Is(err, os.ErrNotExist) {
			return trust, errors.New("managed update trust is not installed; rerun a reviewed native installer")
		}
		if err != nil {
			return trust, fmt.Errorf("read managed update trust: %w", err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&trust); err != nil {
			return trust, fmt.Errorf("decode managed update trust: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return trust, errors.New("managed update trust contains trailing data")
			}
			return trust, fmt.Errorf("decode managed update trust: %w", err)
		}
		if trust.SchemaVersion != updateTrustSchemaVersion {
			return trust, fmt.Errorf("managed update trust schema %d is unsupported", trust.SchemaVersion)
		}
	} else {
		trust = updateTrust{
			SchemaVersion: updateTrustSchemaVersion,
			Channel:       stableUpdateChannel,
			ManifestURL:   cfg.UpdateManifestURL,
			PublicKey:     cfg.UpdatePublicKey,
		}
	}
	if trust.PublicKey == "" || (trust.ManifestURL == "" && trust.ReleaseFeedURL == "") {
		return trust, errors.New("updates are not configured for this installation")
	}
	if trust.Channel != stableUpdateChannel && trust.Channel != testingUpdateChannel {
		return trust, fmt.Errorf("update trust channel %q is unsupported", trust.Channel)
	}
	if trust.ManifestURL != "" && trust.ReleaseFeedURL != "" {
		return trust, errors.New("update trust must configure either manifestUrl or releaseFeedUrl, not both")
	}
	if trust.ManifestURL != "" {
		if err := validateEndpointURL("update manifest URL", trust.ManifestURL, "https", ""); err != nil {
			return trust, err
		}
	}
	if trust.ReleaseFeedURL != "" {
		if trust.Channel != testingUpdateChannel {
			return trust, errors.New("release feed discovery is supported only for the testing update channel")
		}
		if err := validateEndpointURL("update release feed URL", trust.ReleaseFeedURL, "https", ""); err != nil {
			return trust, err
		}
	}
	if _, _, err := decodeUpdateSignature(trust.PublicKey, base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))); err != nil {
		return trust, err
	}
	return trust, nil
}

func validateManagedUpdateTrust(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("managed update trust is not installed; rerun a reviewed native installer")
	}
	if err != nil {
		return fmt.Errorf("inspect managed update trust: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("managed update trust must be a regular file")
	}
	return validateManagedUpdateTrustOwnership(path, info)
}

func fetchUpdateRelease(ctx context.Context, trust updateTrust, client *http.Client) (updateRelease, error) {
	manifestURL := trust.ManifestURL
	if trust.ReleaseFeedURL != "" {
		var err error
		manifestURL, err = discoverTestingManifest(ctx, trust.ReleaseFeedURL, client)
		if err != nil {
			return updateRelease{}, err
		}
	}
	var manifest updateManifest
	if err := getJSON(ctx, client, manifestURL, &manifest); err != nil {
		return updateRelease{}, err
	}
	if manifest.SchemaVersion != updateManifestSchemaVersion {
		return updateRelease{}, fmt.Errorf("update manifest schema %d is unsupported", manifest.SchemaVersion)
	}
	if manifest.Channel != trust.Channel {
		return updateRelease{}, fmt.Errorf("update manifest channel %q does not match trusted channel %q", manifest.Channel, trust.Channel)
	}
	if err := validateUpdateManifestIdentity(manifest); err != nil {
		return updateRelease{}, err
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	artifact, ok := manifest.Artifacts[platform]
	if !ok {
		return updateRelease{}, fmt.Errorf("release %s has no artifact for %s/%s", manifest.Version, runtime.GOOS, runtime.GOARCH)
	}
	publicKey, signature, err := decodeUpdateSignature(trust.PublicKey, artifact.Signature)
	if err != nil {
		return updateRelease{}, err
	}
	statement, err := canonicalUpdateStatement(manifest, platform, artifact)
	if err != nil {
		return updateRelease{}, err
	}
	if !ed25519.Verify(publicKey, statement, signature) {
		return updateRelease{}, errors.New("release metadata signature verification failed")
	}
	if err := validateUpdateDigest(artifact.SHA256); err != nil {
		return updateRelease{}, err
	}
	if err := validateSecureURL("release artifact URL", artifact.URL, "https"); err != nil {
		return updateRelease{}, err
	}
	return updateRelease{Manifest: manifest, Platform: platform, Artifact: artifact}, nil
}

func discoverTestingManifest(ctx context.Context, feedURL string, client *http.Client) (string, error) {
	var releases []releaseFeedItem
	data, err := download(ctx, client, feedURL, maxUpdateReleaseFeedBytes)
	if err != nil {
		return "", fmt.Errorf("read testing release feed: %w", err)
	}
	if err := json.Unmarshal(data, &releases); err != nil {
		return "", fmt.Errorf("decode testing release feed: %w", err)
	}
	bestRun, bestAttempt := -1, -1
	manifestURL := ""
	for _, release := range releases {
		if release.Draft || !release.Prerelease {
			continue
		}
		run, attempt, ok := parseTestingReleaseTag(release.TagName)
		if !ok || run < bestRun || (run == bestRun && attempt <= bestAttempt) {
			continue
		}
		candidateURL := ""
		for _, asset := range release.Assets {
			if asset.Name == "testing.json" {
				candidateURL = asset.BrowserDownloadURL
				break
			}
		}
		if candidateURL == "" {
			continue
		}
		if err := validateSecureURL("testing update manifest URL", candidateURL, "https"); err != nil {
			continue
		}
		bestRun, bestAttempt = run, attempt
		manifestURL = candidateURL
	}
	if manifestURL == "" {
		return "", errors.New("testing release feed contains no signed testing update manifest")
	}
	return manifestURL, nil
}

func parseTestingReleaseTag(tag string) (int, int, bool) {
	parts := strings.Split(tag, "-")
	if len(parts) != 3 || parts[0] != "testing" || !validNumericIdentifier(parts[1]) || !validNumericIdentifier(parts[2]) {
		return 0, 0, false
	}
	run, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	attempt, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, false
	}
	return run, attempt, true
}

func printUpdateRelease(release updateRelease, precedence int) {
	fmt.Printf("Installed version: %s\n", version)
	fmt.Printf("Available version: %s\n", release.Manifest.Version)
	fmt.Printf("Channel: %s\n", release.Manifest.Channel)
	fmt.Printf("Platform: %s\n", release.Platform)
	fmt.Printf("Source: https://github.com/telrad-au/relay/tree/%s\n", release.Manifest.SourceRevision)
	fmt.Printf("Release: https://github.com/telrad-au/relay/releases/tag/%s\n", release.Manifest.ReleaseTag)
	fmt.Printf("Artifact: %s\n", release.Artifact.URL)
	fmt.Printf("SHA-256: %s\n", release.Artifact.SHA256)
	fmt.Println("Release metadata signature: verified")
	if precedence >= 0 {
		fmt.Println("No newer update is available.")
	}
}

func decodeUpdateSignature(encodedPublicKey, encodedSignature string) (ed25519.PublicKey, []byte, error) {
	publicKey, err := base64.StdEncoding.DecodeString(encodedPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, nil, errors.New("updatePublicKey must be a base64 Ed25519 public key")
	}
	signature, err := base64.StdEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, nil, errors.New("release signature is invalid")
	}
	return ed25519.PublicKey(publicKey), signature, nil
}

func verifyUpdateArtifact(publicKey ed25519.PublicKey, manifest updateManifest, platform string, artifact updateArtifact, signature, binary []byte) error {
	manifestDigest := artifact.SHA256
	if err := validateUpdateDigest(manifestDigest); err != nil {
		return err
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("update public key is invalid")
	}
	if len(signature) != ed25519.SignatureSize {
		return errors.New("release signature is invalid")
	}
	digest := sha256.Sum256(binary)
	digestHex := hex.EncodeToString(digest[:])
	if digestHex != manifestDigest {
		return errors.New("release SHA-256 does not match manifest")
	}
	statement, err := canonicalUpdateStatement(manifest, platform, artifact)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, statement, signature) {
		return errors.New("release signature verification failed")
	}
	return nil
}

func validateUpdateDigest(digest string) error {
	if len(digest) != sha256.Size*2 {
		return errors.New("release SHA-256 must be 64 lowercase hexadecimal characters")
	}
	decodedDigest, err := hex.DecodeString(digest)
	if err != nil || hex.EncodeToString(decodedDigest) != digest {
		return errors.New("release SHA-256 must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func canonicalUpdateStatement(manifest updateManifest, platform string, artifact updateArtifact) ([]byte, error) {
	if manifest.SchemaVersion != updateManifestSchemaVersion {
		return nil, errors.New("unsupported update metadata")
	}
	if err := validateUpdateManifestIdentity(manifest); err != nil {
		return nil, err
	}
	if platform == "" || strings.ContainsAny(platform, "\r\n=") {
		return nil, errors.New("release platform is invalid")
	}
	if artifact.URL == "" || strings.ContainsAny(artifact.URL, "\r\n") {
		return nil, errors.New("release artifact URL is invalid")
	}
	if len(artifact.SHA256) != sha256.Size*2 {
		return nil, errors.New("release SHA-256 must be 64 lowercase hexadecimal characters")
	}
	statement := fmt.Sprintf(
		"telrad-relay-update-v2\nchannel=%s\nversion=%s\nreleaseTag=%s\nsourceRevision=%s\nplatform=%s\nurl=%s\nsha256=%s\n",
		manifest.Channel,
		manifest.Version,
		manifest.ReleaseTag,
		manifest.SourceRevision,
		platform,
		artifact.URL,
		artifact.SHA256,
	)
	return []byte(statement), nil
}

func validateUpdateManifestIdentity(manifest updateManifest) error {
	if manifest.Channel != stableUpdateChannel && manifest.Channel != testingUpdateChannel {
		return fmt.Errorf("update manifest channel %q is unsupported", manifest.Channel)
	}
	if _, err := parseReleaseVersion(manifest.Version); err != nil {
		return err
	}
	if len(manifest.SourceRevision) != 40 {
		return errors.New("update source revision must be a 40-character lowercase Git commit")
	}
	if decoded, err := hex.DecodeString(manifest.SourceRevision); err != nil || hex.EncodeToString(decoded) != manifest.SourceRevision {
		return errors.New("update source revision must be a 40-character lowercase Git commit")
	}
	switch manifest.Channel {
	case stableUpdateChannel:
		if manifest.ReleaseTag != "v"+manifest.Version {
			return errors.New("stable update release tag does not match its version")
		}
	case testingUpdateChannel:
		run, attempt, ok := parseTestingReleaseTag(manifest.ReleaseTag)
		if !ok || manifest.Version != fmt.Sprintf("0.0.0-testing.%d.%d", run, attempt) {
			return errors.New("testing update release tag does not match its version")
		}
	}
	return nil
}

func applyStagedUpdate(args []string) error {
	flags := flag.NewFlagSet("apply-update", flag.ContinueOnError)
	target := flags.String("target", "", "target executable")
	parent := flags.Int("parent", 0, "parent process id")
	expectedVersion := flags.String("version", "", "expected release version")
	configPath := flags.String("config", "", "relay configuration path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *target == "" || *parent < 0 || *expectedVersion == "" || *configPath == "" {
		return errors.New("target, parent, version, and config are required")
	}
	self, err := approvedUpdateExecutable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	if *parent > 0 {
		if err := waitForUpdateParent(*parent, 30*time.Second); err != nil {
			return fmt.Errorf("wait for relay process to exit: %w", err)
		}
	}
	fmt.Println("Stopping the Relay service and draining active exchanges...")
	if err := updateServiceAction("stop"); err != nil {
		return fmt.Errorf("stop relay service: %w", err)
	}
	serviceStopped := true
	defer func() {
		if serviceStopped {
			_ = updateServiceAction("start")
		}
	}()
	previous := *target + ".previous"
	current, err := os.ReadFile(*target)
	if err != nil {
		return fmt.Errorf("read current relay executable: %w", err)
	}
	if err := atomicWriteFile(previous, current, 0o755); err != nil {
		return fmt.Errorf("preserve previous relay executable: %w", err)
	}
	activatedAt := time.Now().UTC()
	transaction := updateTransaction{
		Target:          *target,
		Previous:        previous,
		ExpectedVersion: *expectedVersion,
		ConfigPath:      *configPath,
		ActivatedAt:     activatedAt,
		Deadline:        activatedAt.Add(updateHealthTimeout),
	}
	journalPath := updateJournalPath(*target)
	if err := atomicWriteJSON(journalPath, transaction); err != nil {
		return fmt.Errorf("write update transaction journal: %w", err)
	}
	var lastError error
	for attempt := 0; attempt < 60; attempt++ {
		if err := replaceExecutable(*target, binary); err == nil {
			lastError = nil
			break
		} else {
			lastError = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastError != nil {
		_ = os.Remove(journalPath)
		return fmt.Errorf("could not replace relay executable: %w", lastError)
	}
	if err := validateApprovedUpdate(*target, *configPath, *expectedVersion); err != nil {
		serviceStopped = false
		return rollbackUpdateAndRestart(transaction, fmt.Errorf("validate installed update: %w", err))
	}
	fmt.Println("Starting the updated Relay service...")
	if err := updateServiceAction("start"); err != nil {
		serviceStopped = false
		return rollbackUpdateAndRestart(transaction, fmt.Errorf("start updated relay service: %w", err))
	}
	serviceStopped = false
	if err := waitForApprovedUpdate(transaction); err != nil {
		return rollbackUpdateAndRestart(transaction, err)
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("complete update transaction: %w", err)
	}
	_ = os.Remove(previous)
	_ = os.Remove(self)
	if err := syncDirectory(filepath.Dir(*target)); err != nil {
		return err
	}
	fmt.Printf("Relay %s is installed and ready.\n", *expectedVersion)
	return nil
}

func validateInstalledUpdate(target, configPath, expectedVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, target, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run installed relay version check: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != expectedVersion {
		return fmt.Errorf("installed relay reported version %q, expected %q", strings.TrimSpace(string(output)), expectedVersion)
	}
	output, err = exec.CommandContext(ctx, target, "--config", configPath, "doctor").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run installed relay diagnostics: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func waitForUpdatedRuntimeReady(transaction updateTransaction) error {
	timer := time.NewTimer(time.Until(transaction.Deadline))
	defer timer.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		status, err := readRuntimeStatus(transaction.ConfigPath)
		if err == nil && status.State == "ready" && status.IngestReady && !status.AuthenticationAttention && status.Version == transaction.ExpectedVersion && !status.UpdatedAt.Before(transaction.ActivatedAt) {
			return nil
		}
		select {
		case <-timer.C:
			return errors.New("updated relay did not become ready before the rollback deadline")
		case <-ticker.C:
		}
	}
}

func rollbackUpdate(transaction updateTransaction, cause error) error {
	previous, err := os.ReadFile(transaction.Previous)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("read previous relay executable: %w", err))
	}
	var rollbackError error
	for attempt := 0; attempt < 60; attempt++ {
		if err := replaceExecutable(transaction.Target, previous); err == nil {
			rollbackError = nil
			break
		} else {
			rollbackError = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	if rollbackError != nil {
		return errors.Join(cause, fmt.Errorf("restore previous relay executable: %w", rollbackError))
	}
	_ = os.Remove(updateJournalPath(transaction.Target))
	_ = os.Remove(transaction.Previous)
	return cause
}

func rollbackUpdateAndRestart(transaction updateTransaction, cause error) error {
	_ = updateServiceAction("stop")
	rollbackErr := rollbackUpdate(transaction, cause)
	if err := updateServiceAction("start"); err != nil {
		return errors.Join(rollbackErr, fmt.Errorf("restart previous relay service: %w", err))
	}
	return fmt.Errorf("update failed and the previous Relay was restored: %w", rollbackErr)
}

func updateJournalPath(target string) string {
	return target + ".update-transaction.json"
}

func replaceExecutable(target string, binary []byte) error {
	temporary := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".update")
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := atomicWriteFile(temporary, binary, 0o755); err != nil {
		return err
	}
	if err := activateExecutable(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

func getJSON(ctx context.Context, client *http.Client, address string, value any) error {
	data, err := download(ctx, client, address, maxCloudResponseBytes)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func download(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("download exceeds the size limit")
	}
	return data, nil
}
