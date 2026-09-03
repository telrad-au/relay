package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const updateTestSourceRevision = "0123456789abcdef0123456789abcdef01234567"

func TestUpdateCheckVerifiesMetadataWithoutDownloadingOrChangingAnything(t *testing.T) {
	server, cfg, artifactRequests := signedUpdateServer(t, "1.1.0", []byte("approved relay"))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "relay.json")
	originalVersion := version
	version = "1.0.0"
	t.Cleanup(func() { version = originalVersion })

	var updateErr error
	output := captureStandardOutput(t, func() {
		updateErr = updateRelay(context.Background(), cfg, configPath, nil, server.Client())
	})
	if updateErr != nil {
		t.Fatal(updateErr)
	}
	approvalCommand := "sudo telrad update 1.1.0"
	if runtime.GOOS == "windows" {
		approvalCommand = "telrad update 1.1.0"
	}
	for _, expected := range []string{
		"Installed version: 1.0.0",
		"Available version: 1.1.0",
		"Source: https://github.com/telrad-au/relay/tree/" + updateTestSourceRevision,
		"Release: https://github.com/telrad-au/relay/releases/tag/v1.1.0",
		"Release metadata signature: verified",
		"No changes made.",
		approvalCommand,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("update check output %q does not contain %q", output, expected)
		}
	}
	if *artifactRequests != 0 {
		t.Fatalf("check-only update downloaded the artifact %d times", *artifactRequests)
	}
}

func TestUpdateApplyRequiresExactVersionAndStagesVerifiedArtifact(t *testing.T) {
	binary := []byte("approved relay")
	server, cfg, artifactRequests := signedUpdateServer(t, "1.1.0", binary)
	defer server.Close()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	current := filepath.Join(directory, "telrad")
	if err := os.WriteFile(current, []byte("current relay"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteJSON(runtimeStatusPath(configPath), relayRuntimeStatus{
		State:       "ready",
		Version:     "1.0.0",
		UpdatedAt:   time.Now().UTC(),
		ProcessID:   42,
		IngestReady: true,
	}); err != nil {
		t.Fatal(err)
	}
	originalVersion := version
	originalExecutable := approvedUpdateExecutable
	originalStart := startApprovedUpdate
	originalManagedConfig := isManagedUpdateConfig
	version = "1.0.0"
	approvedUpdateExecutable = func() (string, error) { return current, nil }
	isManagedUpdateConfig = func(path string) bool { return path == configPath }
	var launchedPath string
	var launchedArgs []string
	startApprovedUpdate = func(path string, args []string) error {
		launchedPath = path
		launchedArgs = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() {
		version = originalVersion
		approvedUpdateExecutable = originalExecutable
		startApprovedUpdate = originalStart
		isManagedUpdateConfig = originalManagedConfig
	})

	if err := updateRelay(context.Background(), cfg, configPath, []string{"1.0.9"}, server.Client()); err == nil || !strings.Contains(err.Error(), "is not the currently signed release") {
		t.Fatalf("mismatched approval error = %v", err)
	}
	if *artifactRequests != 0 {
		t.Fatal("mismatched approval downloaded the artifact")
	}
	if err := updateRelay(context.Background(), cfg, configPath, []string{"1.1.0"}, server.Client()); err != nil {
		t.Fatal(err)
	}
	if *artifactRequests != 1 {
		t.Fatalf("approved update artifact requests = %d, want 1", *artifactRequests)
	}
	if launchedPath != current+".new" || !strings.Contains(strings.Join(launchedArgs, " "), "-version 1.1.0") {
		t.Fatalf("launched path %q args %q", launchedPath, launchedArgs)
	}
	staged, err := os.ReadFile(current + ".new")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged, binary) {
		t.Fatal("staged update does not match the signed artifact")
	}
}

func signedUpdateServer(t *testing.T, releaseVersion string, binary []byte) (*httptest.Server, *config, *int) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(binary)
	artifactRequests := 0
	var manifest updateManifest
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/stable.json":
			writer.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(writer).Encode(manifest); err != nil {
				t.Error(err)
			}
		case "/telrad":
			artifactRequests++
			_, _ = writer.Write(binary)
		default:
			http.NotFound(writer, request)
		}
	}))
	platform := runtime.GOOS + "-" + runtime.GOARCH
	artifact := updateArtifact{URL: server.URL + "/telrad", SHA256: hex.EncodeToString(digest[:])}
	manifest = updateManifest{
		SchemaVersion:  updateManifestSchemaVersion,
		Channel:        stableUpdateChannel,
		Version:        releaseVersion,
		ReleaseTag:     "v" + releaseVersion,
		SourceRevision: updateTestSourceRevision,
		Artifacts:      map[string]updateArtifact{platform: artifact},
	}
	statement, err := canonicalUpdateStatement(manifest, platform, artifact)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	artifact.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, statement))
	manifest.Artifacts[platform] = artifact
	cfg := defaultConfig()
	cfg.UpdateManifestURL = server.URL + "/stable.json"
	cfg.UpdatePublicKey = base64.StdEncoding.EncodeToString(publicKey)
	return server, cfg, &artifactRequests
}

func TestTestingUpdateDiscoverySelectsNewestSignedReleaseWithoutDownloadingIt(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("testing relay")
	digest := sha256.Sum256(binary)
	artifactRequests := 0
	manifests := map[string]updateManifest{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/releases":
			_ = json.NewEncoder(writer).Encode([]releaseFeedItem{
				{TagName: "v1.0.0-alpha.1", Prerelease: true},
				{TagName: "testing-8-1", Prerelease: true, Assets: []releaseFeedAsset{{Name: "testing.json", BrowserDownloadURL: serverURL(request) + "/testing-8-1/testing.json"}}},
				{TagName: "testing-9-1", Prerelease: true, Assets: []releaseFeedAsset{{Name: "testing.json", BrowserDownloadURL: serverURL(request) + "/testing-9-1/testing.json"}}},
				{TagName: "testing-9-2", Prerelease: true, Assets: []releaseFeedAsset{{Name: "testing.json", BrowserDownloadURL: serverURL(request) + "/testing-9-2/testing.json"}}},
				{TagName: "testing-10-1", Draft: true, Prerelease: true, Assets: []releaseFeedAsset{{Name: "testing.json", BrowserDownloadURL: serverURL(request) + "/testing-10-1/testing.json"}}},
			})
		case "/testing-8-1/testing.json", "/testing-9-1/testing.json", "/testing-9-2/testing.json":
			_ = json.NewEncoder(writer).Encode(manifests[request.URL.Path])
		case "/telrad":
			artifactRequests++
			_, _ = writer.Write(binary)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	for _, release := range []struct {
		tag     string
		version string
	}{
		{tag: "testing-8-1", version: "0.0.0-testing.8.1"},
		{tag: "testing-9-1", version: "0.0.0-testing.9.1"},
		{tag: "testing-9-2", version: "0.0.0-testing.9.2"},
	} {
		artifact := updateArtifact{URL: server.URL + "/telrad", SHA256: hex.EncodeToString(digest[:])}
		manifest := updateManifest{
			SchemaVersion:  updateManifestSchemaVersion,
			Channel:        testingUpdateChannel,
			Version:        release.version,
			ReleaseTag:     release.tag,
			SourceRevision: updateTestSourceRevision,
			Artifacts:      map[string]updateArtifact{platform: artifact},
		}
		statement, err := canonicalUpdateStatement(manifest, platform, artifact)
		if err != nil {
			t.Fatal(err)
		}
		artifact.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, statement))
		manifest.Artifacts[platform] = artifact
		manifests["/"+release.tag+"/testing.json"] = manifest
	}

	trust := updateTrust{
		SchemaVersion:  updateTrustSchemaVersion,
		Channel:        testingUpdateChannel,
		ReleaseFeedURL: server.URL + "/releases",
		PublicKey:      base64.StdEncoding.EncodeToString(publicKey),
	}
	release, err := fetchUpdateRelease(context.Background(), trust, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if release.Manifest.Version != "0.0.0-testing.9.2" || release.Manifest.ReleaseTag != "testing-9-2" {
		t.Fatalf("discovered release = %#v", release.Manifest)
	}
	if artifactRequests != 0 {
		t.Fatalf("testing discovery downloaded the executable %d times", artifactRequests)
	}
}

func TestTestingTrustRejectsStableManifest(t *testing.T) {
	server, cfg, _ := signedUpdateServer(t, "1.1.0", []byte("stable relay"))
	defer server.Close()
	trust := updateTrust{
		SchemaVersion: updateTrustSchemaVersion,
		Channel:       testingUpdateChannel,
		ManifestURL:   cfg.UpdateManifestURL,
		PublicKey:     cfg.UpdatePublicKey,
	}
	if _, err := fetchUpdateRelease(context.Background(), trust, server.Client()); err == nil || !strings.Contains(err.Error(), "does not match trusted channel") {
		t.Fatalf("testing trust accepted stable manifest: %v", err)
	}
}

func TestStableTrustRejectsTestingManifest(t *testing.T) {
	manifest := updateManifest{
		SchemaVersion:  updateManifestSchemaVersion,
		Channel:        testingUpdateChannel,
		Version:        "0.0.0-testing.9.1",
		ReleaseTag:     "testing-9-1",
		SourceRevision: updateTestSourceRevision,
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(manifest)
	}))
	defer server.Close()
	trust := updateTrust{
		SchemaVersion: updateTrustSchemaVersion,
		Channel:       stableUpdateChannel,
		ManifestURL:   server.URL + "/stable.json",
		PublicKey:     base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)),
	}
	if _, err := fetchUpdateRelease(context.Background(), trust, server.Client()); err == nil || !strings.Contains(err.Error(), "does not match trusted channel") {
		t.Fatalf("stable trust accepted testing manifest: %v", err)
	}
}

func TestParseTestingReleaseTag(t *testing.T) {
	for _, test := range []struct {
		tag         string
		run         int
		attempt     int
		shouldParse bool
	}{
		{tag: "testing-42-1", run: 42, attempt: 1, shouldParse: true},
		{tag: "testing-42-2", run: 42, attempt: 2, shouldParse: true},
		{tag: "testing-042-1"},
		{tag: "v0.0.0-testing.42.1"},
		{tag: "testing-42"},
	} {
		run, attempt, ok := parseTestingReleaseTag(test.tag)
		if ok != test.shouldParse || run != test.run || attempt != test.attempt {
			t.Errorf("parseTestingReleaseTag(%q) = %d, %d, %v", test.tag, run, attempt, ok)
		}
	}
}

func serverURL(request *http.Request) string {
	return "https://" + request.Host
}

func TestVerifyUpdateArtifact(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("signed relay release")
	digest := sha256.Sum256(binary)
	digestHex := hex.EncodeToString(digest[:])
	manifest := updateManifest{SchemaVersion: updateManifestSchemaVersion, Channel: stableUpdateChannel, Version: "2.1.0", ReleaseTag: "v2.1.0", SourceRevision: updateTestSourceRevision}
	artifact := updateArtifact{URL: "https://downloads.example.test/v2.1.0/relay", SHA256: digestHex}
	statement, err := canonicalUpdateStatement(manifest, "linux-amd64", artifact)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, statement)

	tests := []struct {
		name      string
		publicKey ed25519.PublicKey
		manifest  updateManifest
		platform  string
		artifact  updateArtifact
		signature []byte
		binary    []byte
		wantError bool
	}{
		{name: "valid", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: artifact, signature: signature, binary: binary},
		{name: "modified binary", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: artifact, signature: signature, binary: []byte("modified relay release"), wantError: true},
		{name: "modified digest", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: updateArtifact{URL: artifact.URL, SHA256: strings.Repeat("0", sha256.Size*2)}, signature: signature, binary: binary, wantError: true},
		{name: "modified version", publicKey: publicKey, manifest: updateManifest{SchemaVersion: updateManifestSchemaVersion, Channel: stableUpdateChannel, Version: "2.2.0", ReleaseTag: "v2.2.0", SourceRevision: updateTestSourceRevision}, platform: "linux-amd64", artifact: artifact, signature: signature, binary: binary, wantError: true},
		{name: "modified release tag", publicKey: publicKey, manifest: updateManifest{SchemaVersion: updateManifestSchemaVersion, Channel: stableUpdateChannel, Version: "2.1.0", ReleaseTag: "v2.1.1", SourceRevision: updateTestSourceRevision}, platform: "linux-amd64", artifact: artifact, signature: signature, binary: binary, wantError: true},
		{name: "modified source revision", publicKey: publicKey, manifest: updateManifest{SchemaVersion: updateManifestSchemaVersion, Channel: stableUpdateChannel, Version: "2.1.0", ReleaseTag: "v2.1.0", SourceRevision: strings.Repeat("a", 40)}, platform: "linux-amd64", artifact: artifact, signature: signature, binary: binary, wantError: true},
		{name: "modified platform", publicKey: publicKey, manifest: manifest, platform: "windows-amd64", artifact: artifact, signature: signature, binary: binary, wantError: true},
		{name: "modified URL", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: updateArtifact{URL: "https://attacker.example/artifact", SHA256: digestHex}, signature: signature, binary: binary, wantError: true},
		{name: "modified signature", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: artifact, signature: modifiedCopy(signature), binary: binary, wantError: true},
		{name: "uppercase digest", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: updateArtifact{URL: artifact.URL, SHA256: strings.ToUpper(digestHex)}, signature: signature, binary: binary, wantError: true},
		{name: "trailing newline", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: updateArtifact{URL: artifact.URL, SHA256: digestHex + "\n"}, signature: signature, binary: binary, wantError: true},
		{name: "malformed digest", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: updateArtifact{URL: artifact.URL, SHA256: strings.Repeat("z", sha256.Size*2)}, signature: signature, binary: binary, wantError: true},
		{name: "invalid public key", publicKey: ed25519.PublicKey("short"), manifest: manifest, platform: "linux-amd64", artifact: artifact, signature: signature, binary: binary, wantError: true},
		{name: "invalid signature", publicKey: publicKey, manifest: manifest, platform: "linux-amd64", artifact: artifact, signature: []byte("short"), binary: binary, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyUpdateArtifact(test.publicKey, test.manifest, test.platform, test.artifact, test.signature, test.binary)
			if (err != nil) != test.wantError {
				t.Fatalf("verifyUpdateArtifact() error = %v, wantError %v", err, test.wantError)
			}
		})
	}
}

func TestDecodeUpdateSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, []byte("digest"))
	encodedPublicKey := base64.StdEncoding.EncodeToString(publicKey)
	encodedSignature := base64.StdEncoding.EncodeToString(signature)

	if _, _, err := decodeUpdateSignature(encodedPublicKey, encodedSignature); err != nil {
		t.Fatalf("valid update signature metadata was rejected: %v", err)
	}
	for _, test := range []struct {
		name      string
		publicKey string
		signature string
	}{
		{name: "malformed public key base64", publicKey: "%%%", signature: encodedSignature},
		{name: "wrong public key size", publicKey: base64.StdEncoding.EncodeToString([]byte("short")), signature: encodedSignature},
		{name: "malformed signature base64", publicKey: encodedPublicKey, signature: "%%%"},
		{name: "wrong signature size", publicKey: encodedPublicKey, signature: base64.StdEncoding.EncodeToString([]byte("short"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := decodeUpdateSignature(test.publicKey, test.signature); err == nil {
				t.Fatal("invalid update signature metadata was accepted")
			}
		})
	}
}

func TestWaitForUpdatedRuntimeReadyRequiresExpectedFreshVersion(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	activatedAt := time.Now().UTC()
	transaction := updateTransaction{
		ConfigPath:      configPath,
		ExpectedVersion: "2.0.0",
		ActivatedAt:     activatedAt,
		Deadline:        activatedAt.Add(time.Second),
	}
	if err := atomicWriteJSON(runtimeStatusPath(configPath), relayRuntimeStatus{
		State:       "ready",
		Version:     transaction.ExpectedVersion,
		UpdatedAt:   activatedAt.Add(time.Millisecond),
		ProcessID:   42,
		IngestReady: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := waitForUpdatedRuntimeReady(transaction); err != nil {
		t.Fatalf("fresh expected runtime status was rejected: %v", err)
	}

	transaction.Deadline = time.Now().Add(20 * time.Millisecond)
	if err := atomicWriteJSON(runtimeStatusPath(configPath), relayRuntimeStatus{
		State:       "ready",
		Version:     "1.9.0",
		UpdatedAt:   time.Now().UTC(),
		ProcessID:   43,
		IngestReady: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := waitForUpdatedRuntimeReady(transaction); err == nil {
		t.Fatal("ready status from the wrong version was accepted")
	}
}

func TestRollbackUpdateRestoresPreviousExecutable(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "telrad")
	previous := target + ".previous"
	journal := updateJournalPath(target)
	if err := os.WriteFile(target, []byte("broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("working"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("health check failed")
	if err := rollbackUpdate(updateTransaction{Target: target, Previous: previous}, cause); !errors.Is(err, cause) {
		t.Fatalf("rollbackUpdate() error = %v", err)
	}
	restored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != "working" {
		t.Fatalf("restored executable = %q", restored)
	}
	if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update journal remains after rollback: %v", err)
	}
}

func TestGeneratedReleaseBundle(t *testing.T) {
	releaseDir := os.Getenv("RELAY_RELEASE_VERIFY_DIR")
	if releaseDir == "" {
		t.Skip("RELAY_RELEASE_VERIFY_DIR is not set")
	}
	manifestData, err := os.ReadFile(filepath.Join(releaseDir, "stable.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest updateManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != updateManifestSchemaVersion || manifest.Channel != stableUpdateChannel {
		t.Fatal("release manifest has unsupported schema or channel metadata")
	}
	if err := validateUpdateManifestIdentity(manifest); err != nil {
		t.Fatalf("release identity is invalid: %v", err)
	}
	publicKeyData, err := os.ReadFile(filepath.Join(releaseDir, "update-public-key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	encodedPublicKey := strings.TrimSpace(string(publicKeyData))
	windowsSignerPinData, err := os.ReadFile(filepath.Join(releaseDir, "windows-signer-certificate-sha256.txt"))
	if err != nil {
		t.Fatal(err)
	}
	windowsSignerPin := strings.TrimSpace(string(windowsSignerPinData))
	if len(windowsSignerPin) != sha256.Size*2 || strings.ToLower(windowsSignerPin) != windowsSignerPin {
		t.Fatal("Windows signer certificate pin is not a canonical SHA-256 digest")
	}
	if _, err := hex.DecodeString(windowsSignerPin); err != nil {
		t.Fatalf("Windows signer certificate pin is malformed: %v", err)
	}
	artifacts := map[string]string{
		"linux-amd64":   "telrad-relay-linux-amd64",
		"linux-arm64":   "telrad-relay-linux-arm64",
		"windows-amd64": "telrad-relay-windows-amd64.exe",
	}
	for platform, filename := range artifacts {
		t.Run(platform, func(t *testing.T) {
			artifact, ok := manifest.Artifacts[platform]
			if !ok {
				t.Fatalf("manifest has no artifact for %s", platform)
			}
			binary, err := os.ReadFile(filepath.Join(releaseDir, filename))
			if err != nil {
				t.Fatal(err)
			}
			publicKey, signature, err := decodeUpdateSignature(encodedPublicKey, artifact.Signature)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyUpdateArtifact(publicKey, manifest, platform, artifact, signature, binary); err != nil {
				t.Fatalf("generated release artifact verification failed: %v", err)
			}
		})
	}

	windowsInstallerData, err := os.ReadFile(filepath.Join(releaseDir, "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	windowsInstaller := string(windowsInstallerData)
	if !strings.Contains(windowsInstaller, `$TrustedUpdatePublicKey = "`+encodedPublicKey+`"`) {
		t.Fatal("Windows installer does not pin the release update public key")
	}
	if !strings.Contains(windowsInstaller, `$TrustedWindowsSignerCertificateSha256 = "`+windowsSignerPin+`"`) {
		t.Fatal("Windows installer does not pin the Authenticode signer certificate")
	}
	if strings.Contains(windowsInstaller, "@@") || strings.Contains(windowsInstaller, "/update-public-key.txt") {
		t.Fatal("Windows installer contains an unresolved or remotely downloaded trust root")
	}
	if !strings.Contains(windowsInstaller, `channel = "stable"`) || !strings.Contains(windowsInstaller, `manifestUrl = $UpdateManifestUrl`) || !strings.Contains(windowsInstaller, `Join-Path $target "update-trust.json"`) || strings.Contains(windowsInstaller, `$ArtifactBaseUrl/stable.json`) {
		t.Fatal("Windows installer does not install separate administrator-owned update trust")
	}

	linuxInstallerData, err := os.ReadFile(filepath.Join(releaseDir, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	linuxInstaller := string(linuxInstallerData)
	if !strings.Contains(linuxInstaller, `UPDATE_PUBLIC_KEY="`+encodedPublicKey+`"`) {
		t.Fatal("Linux installer does not pin the release update public key")
	}
	if strings.Contains(linuxInstaller, "@@") || strings.Contains(linuxInstaller, `$ARTIFACT_BASE_URL/update-public-key.pem`) || strings.Contains(linuxInstaller, `$ARTIFACT_BASE_URL/update-public-key.txt`) {
		t.Fatal("Linux installer contains an unresolved or remotely downloaded trust root")
	}
	if !strings.Contains(linuxInstaller, `"channel": "stable"`) || !strings.Contains(linuxInstaller, `"manifestUrl": "$UPDATE_MANIFEST_URL"`) || !strings.Contains(linuxInstaller, `/usr/local/lib/telrad-relay/update-trust.json`) || strings.Contains(linuxInstaller, `$ARTIFACT_BASE_URL/stable.json`) {
		t.Fatal("Linux installer does not install separate administrator-owned update trust")
	}
	if !strings.Contains(linuxInstaller, `install -m 0755 -o root -g root "$work_dir/telrad-relay" /usr/local/lib/telrad-relay/telrad`) {
		t.Fatal("Linux installer does not make the executable administrator-owned")
	}
	if !strings.Contains(linuxInstaller, `aarch64|arm64) artifact_arch="arm64"`) || !strings.Contains(linuxInstaller, `telrad-relay-linux-$artifact_arch`) {
		t.Fatal("Linux installer does not select the release artifact for the host architecture")
	}
	for _, filename := range []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md"} {
		contents, err := os.ReadFile(filepath.Join(releaseDir, filename))
		if err != nil {
			t.Fatalf("release bundle is missing %s: %v", filename, err)
		}
		if len(contents) == 0 {
			t.Fatalf("release bundle contains an empty %s", filename)
		}
		sourceContents, err := os.ReadFile(filepath.Join("..", "..", filename))
		if err != nil {
			t.Fatalf("read source %s: %v", filename, err)
		}
		if !bytes.Equal(contents, sourceContents) {
			t.Fatalf("release bundle contains a modified %s", filename)
		}
	}
	installationData, err := os.ReadFile(filepath.Join(releaseDir, "installation-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var installation struct {
		SchemaVersion  int            `json:"schemaVersion"`
		ReleaseVersion string         `json:"releaseVersion"`
		Components     map[string]int `json:"components"`
	}
	if err := json.Unmarshal(installationData, &installation); err != nil {
		t.Fatal(err)
	}
	if installation.SchemaVersion != 1 || installation.ReleaseVersion == "" {
		t.Fatal("installation manifest has invalid version metadata")
	}
	if installation.Components["configuration"] != currentConfigSchemaVersion {
		t.Fatalf("installation manifest configuration component = %d, want %d", installation.Components["configuration"], currentConfigSchemaVersion)
	}
	for _, component := range []string{"configuration", "linuxService", "windowsService", "windowsFirewall", "updateTrust"} {
		if installation.Components[component] < 1 {
			t.Fatalf("installation manifest does not version %s", component)
		}
	}
}

func TestGeneratedDevelopmentReleaseBundle(t *testing.T) {
	releaseDir := os.Getenv("RELAY_DEVELOPMENT_RELEASE_VERIFY_DIR")
	if releaseDir == "" {
		t.Skip("RELAY_DEVELOPMENT_RELEASE_VERIFY_DIR is not set")
	}
	manifestData, err := os.ReadFile(filepath.Join(releaseDir, "stable.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest updateManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != updateManifestSchemaVersion || manifest.Channel != stableUpdateChannel {
		t.Fatal("development release manifest has unsupported schema or channel metadata")
	}
	if err := validateUpdateManifestIdentity(manifest); err != nil {
		t.Fatalf("development release identity is invalid: %v", err)
	}
	if parsed, err := parseReleaseVersion(manifest.Version); err != nil || len(parsed.prerelease) == 0 {
		t.Fatalf("development release manifest version %q is not prerelease SemVer: %v", manifest.Version, err)
	}
	if len(manifest.Artifacts) != 2 {
		t.Fatalf("development release manifest has %d artifacts, want two Linux artifacts", len(manifest.Artifacts))
	}
	publicKeyData, err := os.ReadFile(filepath.Join(releaseDir, "update-public-key.txt"))
	if err != nil {
		t.Fatal(err)
	}
	encodedPublicKey := strings.TrimSpace(string(publicKeyData))
	publicKey, err := base64.StdEncoding.DecodeString(encodedPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		t.Fatalf("development update public key is invalid: %v", err)
	}

	artifacts := map[string]string{
		"linux-amd64": "telrad-relay-linux-amd64",
		"linux-arm64": "telrad-relay-linux-arm64",
	}
	for platform, filename := range artifacts {
		t.Run(platform, func(t *testing.T) {
			artifact, ok := manifest.Artifacts[platform]
			if !ok {
				t.Fatalf("development manifest has no artifact for %s", platform)
			}
			if !strings.HasPrefix(artifact.URL, "https://") || !strings.HasSuffix(artifact.URL, "/"+filename) {
				t.Fatalf("development artifact URL %q is not the expected HTTPS filename", artifact.URL)
			}
			binary, err := os.ReadFile(filepath.Join(releaseDir, filename))
			if err != nil {
				t.Fatal(err)
			}
			manifestPublicKey, manifestSignature, err := decodeUpdateSignature(encodedPublicKey, artifact.Signature)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyUpdateArtifact(manifestPublicKey, manifest, platform, artifact, manifestSignature, binary); err != nil {
				t.Fatalf("development manifest verification failed: %v", err)
			}

			digestData, err := os.ReadFile(filepath.Join(releaseDir, filename+".sha256"))
			if err != nil {
				t.Fatal(err)
			}
			if string(digestData) != artifact.SHA256 || len(digestData) != sha256.Size*2 {
				t.Fatal("development detached digest is not the canonical manifest digest")
			}
			detachedSignature, err := os.ReadFile(filepath.Join(releaseDir, filename+".sig"))
			if err != nil {
				t.Fatal(err)
			}
			if !ed25519.Verify(ed25519.PublicKey(publicKey), digestData, detachedSignature) {
				t.Fatal("development detached digest signature is invalid")
			}
		})
	}

	linuxInstallerData, err := os.ReadFile(filepath.Join(releaseDir, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	linuxInstaller := string(linuxInstallerData)
	for _, required := range []string{
		`ARTIFACT_BASE_URL="https://`,
		`UPDATE_MANIFEST_URL="https://`,
		`PAIRING_URL="https://`,
		`UPDATE_PUBLIC_KEY="` + encodedPublicKey + `"`,
		`/usr/local/lib/telrad-relay/update-trust.json`,
		`"channel": "stable"`,
		`"manifestUrl": "$UPDATE_MANIFEST_URL"`,
		`aarch64|arm64) artifact_arch="arm64"`,
		`telrad-relay-linux-$artifact_arch`,
	} {
		if !strings.Contains(linuxInstaller, required) {
			t.Fatalf("development Linux installer does not contain %q", required)
		}
	}
	if strings.Contains(linuxInstaller, "@@") || strings.Contains(linuxInstaller, `$ARTIFACT_BASE_URL/update-public-key.pem`) || strings.Contains(linuxInstaller, `$ARTIFACT_BASE_URL/update-public-key.txt`) {
		t.Fatal("development Linux installer contains an unresolved or remotely downloaded trust root")
	}
	for _, filename := range []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md", "telrad-relay.service", "installation-manifest.json"} {
		contents, err := os.ReadFile(filepath.Join(releaseDir, filename))
		if err != nil {
			t.Fatalf("development release bundle is missing %s: %v", filename, err)
		}
		if len(contents) == 0 {
			t.Fatalf("development release bundle contains an empty %s", filename)
		}
	}
}

func TestGeneratedTestingReleaseBundle(t *testing.T) {
	releaseDir := os.Getenv("RELAY_TESTING_RELEASE_VERIFY_DIR")
	if releaseDir == "" {
		t.Skip("RELAY_TESTING_RELEASE_VERIFY_DIR is not set")
	}
	manifestData, err := os.ReadFile(filepath.Join(releaseDir, "testing.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest updateManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != updateManifestSchemaVersion || manifest.Channel != testingUpdateChannel {
		t.Fatal("testing release manifest has unsupported schema or channel metadata")
	}
	if err := validateUpdateManifestIdentity(manifest); err != nil {
		t.Fatalf("testing release identity is invalid: %v", err)
	}

	trustData, err := os.ReadFile(filepath.Join(releaseDir, "update-trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	var trust updateTrust
	if err := json.Unmarshal(trustData, &trust); err != nil {
		t.Fatal(err)
	}
	if trust.SchemaVersion != updateTrustSchemaVersion || trust.Channel != testingUpdateChannel || trust.ManifestURL != "" {
		t.Fatalf("testing update trust is invalid: %#v", trust)
	}
	if trust.ReleaseFeedURL != "https://api.github.com/repos/telrad-au/relay/releases" {
		t.Fatalf("testing release feed URL = %q", trust.ReleaseFeedURL)
	}
	publicKey, err := base64.StdEncoding.DecodeString(trust.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		t.Fatalf("testing update public key is invalid: %v", err)
	}

	artifacts := map[string]string{
		"linux-amd64":   "telrad-relay-linux-amd64",
		"linux-arm64":   "telrad-relay-linux-arm64",
		"windows-amd64": "telrad-relay-windows-amd64.exe",
	}
	for platform, filename := range artifacts {
		t.Run(platform, func(t *testing.T) {
			artifact, ok := manifest.Artifacts[platform]
			if !ok {
				t.Fatalf("testing manifest has no artifact for %s", platform)
			}
			binary, err := os.ReadFile(filepath.Join(releaseDir, filename))
			if err != nil {
				t.Fatal(err)
			}
			manifestPublicKey, signature, err := decodeUpdateSignature(trust.PublicKey, artifact.Signature)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyUpdateArtifact(manifestPublicKey, manifest, platform, artifact, signature, binary); err != nil {
				t.Fatalf("testing manifest verification failed: %v", err)
			}
			signatureData, err := os.ReadFile(filepath.Join(releaseDir, filename+".update.sig"))
			if err != nil {
				t.Fatal(err)
			}
			statement, err := canonicalUpdateStatement(manifest, platform, artifact)
			if err != nil {
				t.Fatal(err)
			}
			if !ed25519.Verify(ed25519.PublicKey(publicKey), statement, signatureData) {
				t.Fatal("detached testing update statement signature is invalid")
			}
		})
	}
}

func modifiedCopy(value []byte) []byte {
	copyOfValue := append([]byte(nil), value...)
	copyOfValue[0] ^= 0xff
	return copyOfValue
}
