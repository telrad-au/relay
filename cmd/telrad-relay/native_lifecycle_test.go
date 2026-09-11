//go:build !relay_container

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
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This test deliberately uses the real installed service and protected paths.
// The opt-in scripts run it only on disposable Linux/Windows CI hosts.
func TestNativeInstalledLifecycle(t *testing.T) {
	if os.Getenv("TELRAD_NATIVE_LIFECYCLE_TEST") != "1" {
		t.Skip("requires a disposable native CI host")
	}
	if !platformAdministrator() {
		t.Fatal("native fixture setup requires an administrator")
	}
	p := nativePaths()
	measure := measureNativeLifecycle(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	run := func(input io.Reader, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, p.Executable, args...)
		command.Stdin = input
		return command.CombinedOutput()
	}
	mustRun := func(args ...string) {
		t.Helper()
		if output, err := run(nil, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output)
		}
	}
	mustRun("native-action", "stop")
	t.Cleanup(func() { _ = serviceAction("stop") })
	var paired atomic.Int32
	renewed := make(chan struct{}, 1)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/relay/device-authorizations":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"requestId":"native-ci","deviceSecret":%q,"verificationUri":"https://example.invalid/approve","expiresAt":%q,"intervalSeconds":3}`, strings.Repeat("s", 40), time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/native-ci/token"):
			fmt.Fprintf(w, `{"pairingToken":%q}`, strings.Repeat("t", 40))
		case r.URL.Path == "/v1/relay/pairing-enrollments":
			identity := paired.Add(1)
			now := time.Now().UTC()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"relayId":"native-ci-%d","protocolVersion":1,"credentialVersion":2,"familyId":"native-family-%d","generation":1,"accessCredential":%q,"accessExpiresAt":%q,"renewableCredential":%q,"renewableExpiresAt":%q,"renewalUrl":%q}`,
				identity, identity, testAccessCredential(byte('A'+identity)), now.Add(10*time.Minute).Format(time.RFC3339Nano), testRenewableCredential(byte('A'+identity)), now.Add(30*24*time.Hour).Format(time.RFC3339Nano), server.URL+"/v1/relay/credentials/renew")
		case r.URL.Path == "/v1/relay/credentials/renew":
			now := time.Now().UTC()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"credentialVersion":2,"familyId":"native-family-1","generation":2,"accessCredential":%q,"accessExpiresAt":%q,"renewableCredential":%q,"renewableExpiresAt":%q}`,
				testAccessCredential('R'), now.Add(10*time.Minute).Format(time.RFC3339Nano), testRenewableCredential('R'), now.Add(30*24*time.Hour).Format(time.RFC3339Nano))
		case r.URL.Path == "/v1/relay/control/sessions":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(readyMessage{Type: "ready", SessionID: "native-session", Transports: map[string]readyTransport{"dicom": {URL: server.URL + "/v1/relay/ingest/dicom", ContentType: "application/dicom"}, "hl7": {URL: server.URL + "/v1/relay/ingest/hl7", ContentType: "application/hl7-v2"}}})
		default:
			if r.Header.Get("Authorization") == "Bearer "+testAccessCredential('R') {
				select {
				case renewed <- struct{}{}:
				default:
				}
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	caPath := filepath.Join(filepath.Dir(p.Executable), "native-ci-ca.pem")
	if err := safeAtomicWrite(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	configureNativeTestCA(t, caPath, server.Certificate().Raw)
	configBytes, _ := json.Marshal(defaultConfig())
	var cfg config
	json.Unmarshal(configBytes, &cfg)
	cfg.PairingURL = server.URL + "/v1/relay/pairing-enrollments"
	cfg.ListenAddress = "127.0.0.1"
	cfg.DicomPort, cfg.HL7Port = 21112, 22575
	data, _ := json.Marshal(cfg)
	if err := safeAtomicWrite(p.Config, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := secureNativeState(); err != nil {
		t.Fatal(err)
	}
	startupStarted := time.Now()
	mustRun("auth")
	waitReady := func(want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			state, err := readRuntimeStatus(p.Config)
			if err == nil && state.Version == want && state.IngestReady && state.ControlConnected && !state.AuthenticationAttention {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("installed service did not become ready at %s", want)
	}
	waitReady("0.0.0-ci.1")
	measure("startup-and-pairing", startupStarted)
	if os.Getenv("TELRAD_PERF_LIFECYCLE_OUT") != "" {
		started := time.Now()
		mustRun("native-action", "restart")
		waitReady("0.0.0-ci.1")
		measure("restart", started)
	}
	mustRun("rotate-credential")
	select {
	case <-renewed:
	case <-time.After(15 * time.Second):
		t.Fatal("running service did not adopt renewed credential")
	}
	mustRun("enroll")
	waitReady("0.0.0-ci.1")
	loaded, err := loadConfigMode(p.Config, false)
	if err != nil || loaded.RelayID != "native-ci-2" {
		t.Fatal("re-pairing did not adopt replacement identity")
	}
	credentialBefore, err := safeReadFile(p.Credential, maxCloudResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(os.Getenv("TELRAD_NATIVE_NEXT_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := updateTrust{SchemaVersion: 1, Channel: stableUpdateChannel, ManifestURL: "https://example.invalid/stable.json", PublicKey: base64.StdEncoding.EncodeToString(public)}
	data, _ = json.Marshal(trust)
	if err := safeAtomicWrite(p.Trust, data, 0644); err != nil {
		t.Fatal(err)
	}
	approve := func(candidate string) ([]byte, error) {
		platform := runtime.GOOS + "-" + runtime.GOARCH
		digest := sha256.Sum256(artifact)
		item := updateArtifact{URL: "https://example.invalid/telrad", SHA256: hex.EncodeToString(digest[:])}
		manifest := updateManifest{SchemaVersion: updateManifestSchemaVersion, Channel: stableUpdateChannel, Version: candidate, ReleaseTag: "v" + candidate, SourceRevision: updateTestSourceRevision, Artifacts: map[string]updateArtifact{platform: item}}
		statement, err := canonicalUpdateStatement(manifest, platform, item)
		if err != nil {
			t.Fatal(err)
		}
		item.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(private, statement))
		manifest.Artifacts[platform] = item
		payload, err := approvedPayloadReader(updateRelease{Manifest: manifest, Platform: platform}, artifact)
		if err != nil {
			t.Fatal(err)
		}
		return run(payload, "native-action", "update", candidate)
	}
	updateStarted := time.Now()
	if output, err := approve("0.0.0-ci.2"); err != nil {
		t.Fatalf("signed native update: %v\n%s", err, output)
	}
	waitReady("0.0.0-ci.2")
	measure("signed-update", updateStarted)
	// A failed approved update must finish rollback before the command returns.
	rollbackStarted := time.Now()
	if output, err := approve("0.0.0-ci.3"); err == nil || !bytes.Contains(output, []byte("previous Relay was restored")) {
		t.Fatalf("failed update did not return its rollback result: %v %s", err, output)
	}
	if _, err := os.Lstat(updateJournalPath(p.Executable)); !os.IsNotExist(err) {
		t.Fatal("update returned before rollback completed")
	}
	if output, err := run(nil, "version"); err != nil || strings.TrimSpace(string(output)) != "0.0.0-ci.2" {
		t.Fatal("failed update did not restore the previous executable before returning")
	}
	waitReady("0.0.0-ci.2")
	measure("automatic-rollback", rollbackStarted)
	// Recreate the persistent evidence an interrupted updater leaves. Neither
	// update refusal nor reviewed reinstallation may use its nominated target.
	sentinel := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(sentinel, []byte("outside sentinel"), 0644); err != nil {
		t.Fatal(err)
	}
	interrupted, _ := json.Marshal(map[string]string{"target": sentinel, "configPath": sentinel})
	if err := safeAtomicWrite(updateJournalPath(p.Executable), interrupted, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := approve("0.0.0-ci.3"); err == nil || !bytes.Contains(output, []byte("interrupted update")) {
		t.Fatalf("interrupted update accepted: %v %s", err, output)
	}
	repair, _ := json.Marshal(nativeInstallInput{Config: *defaultConfig(), Trust: trust, Installation: json.RawMessage(`{"schemaVersion":1,"releaseVersion":"0.0.0-ci.2"}`)})
	reinstaller := exec.CommandContext(ctx, os.Getenv("TELRAD_NATIVE_NEXT_BINARY"), "install-native")
	reinstaller.Stdin = bytes.NewReader(repair)
	if output, err := reinstaller.CombinedOutput(); err != nil {
		t.Fatalf("repair interrupted update: %v %s", err, output)
	}
	if _, err := os.Lstat(updateJournalPath(p.Executable)); !os.IsNotExist(err) {
		t.Fatal("repair retained interrupted update state")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside sentinel" {
		t.Fatal("repair followed an injected updater path")
	}
	waitReady("0.0.0-ci.2")
	credentialAfter, _ := safeReadFile(p.Credential, maxCloudResponseBytes)
	if !bytes.Equal(credentialBefore, credentialAfter) {
		t.Fatal("update or rollback modified valid enrollment")
	}
	mustRun("native-action", "stop")
	before, _ := safeReadFile(p.Config, maxCloudResponseBytes)
	for _, action := range []string{"status", "doctor", "ready"} {
		if _, err := run(nil, action); err == nil {
			t.Fatalf("%s succeeded while service was stopped", action)
		}
	}
	after, _ := safeReadFile(p.Config, maxCloudResponseBytes)
	if !bytes.Equal(before, after) {
		t.Fatal("stopped read-only diagnostics modified configuration")
	}
	checkSpoofedNativeEndpoint(t)
}
