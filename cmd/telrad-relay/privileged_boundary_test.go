//go:build !relay_container

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveryRejectsInjectedPathsBeforeChangingFiles(t *testing.T) {
	for _, field := range []string{"configPath", "credentialPath", "configNext", "credentialNext", "configBackup", "credentialBackup"} {
		for _, kind := range []string{"absolute", "traversal", "sibling"} {
			t.Run(field+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				state := filepath.Join(root, "state")
				if err := os.Mkdir(state, 0700); err != nil {
					t.Fatal(err)
				}
				configPath := filepath.Join(state, "relay.json")
				credential := filepath.Join(state, "relay-credential.json")
				sentinel := filepath.Join(root, "outside")
				original := []byte("outside must survive")
				os.WriteFile(sentinel, original, 0600)
				transaction := map[string]any{"configPath": configPath, "credentialPath": credential, "configNext": configPath + ".next", "credentialNext": credential + ".next", "configBackup": configPath + ".previous", "credentialBackup": credential + ".previous", "hadConfig": false, "hadCredential": false}
				injected := sentinel
				if kind == "traversal" {
					injected = state + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "outside"
				}
				if kind == "sibling" {
					injected = state + "-sibling/outside"
				}
				transaction[field] = injected
				encoded, _ := json.Marshal(transaction)
				journal := pairingJournalPath(configPath)
				os.WriteFile(journal, encoded, 0600)
				if err := recoverPairingTransaction(configPath); err == nil {
					t.Fatal("malicious recovery accepted")
				}
				actual, err := os.ReadFile(sentinel)
				if err != nil || !bytes.Equal(actual, original) {
					t.Fatal("outside sentinel changed")
				}
				actual, err = os.ReadFile(journal)
				if err != nil || !bytes.Equal(actual, encoded) {
					t.Fatal("rejected journal changed")
				}
			})
		}
	}
}

func TestRecoveryDoesNotReadCredentialCleanupPathsFromConfig(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	os.Mkdir(state, 0700)
	outside := filepath.Join(root, "outside.next")
	os.WriteFile(outside, []byte("retain"), 0644)
	configPath := filepath.Join(state, "relay.json")
	data, _ := json.Marshal(map[string]any{"credentialPath": filepath.Join(root, "outside")})
	os.WriteFile(configPath, data, 0600)
	if err := recoverPairingTransaction(configPath); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "retain" {
		t.Fatal("config redirected cleanup")
	}
}

func TestSafeFilesRejectLinksAndDoNotChangeOutsideMetadata(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, "state")
			os.Mkdir(state, 0700)
			outside := filepath.Join(root, "outside")
			os.WriteFile(outside, []byte("keep"), 0644)
			before, _ := os.Stat(outside)
			link := filepath.Join(state, "relay.json")
			var err error
			if kind == "symlink" {
				err = os.Symlink(outside, link)
			} else {
				err = os.Link(outside, link)
			}
			if err != nil {
				t.Skipf("link unavailable: %v", err)
			}
			if _, err := safeReadFile(link, 1024); err == nil {
				t.Fatal("read followed a link")
			}
			if err := safeAtomicWrite(link, []byte("changed"), 0600); err == nil {
				t.Fatal("write accepted link")
			}
			if err := safeRemove(link); err == nil {
				t.Fatal("cleanup accepted link")
			}
			data, err := os.ReadFile(outside)
			after, _ := os.Stat(outside)
			if err != nil || string(data) != "keep" || before.Mode() != after.Mode() {
				t.Fatal("outside file or mode changed")
			}
		})
	}
}

func TestReadOnlyCommandsLeaveRecoveryAndOldConfigUntouched(t *testing.T) {
	for _, action := range []string{"version", "doctor", "ready", "update"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "relay.json")
			cfg := defaultConfig()
			cfg.SchemaVersion = 3
			encoded, _ := json.Marshal(cfg)
			os.WriteFile(path, encoded, 0600)
			journal := pairingJournalPath(path)
			os.WriteFile(journal, []byte("malformed journal"), 0600)
			next := path + ".next"
			os.WriteFile(next, []byte("pending"), 0600)
			_ = execute([]string{"--config", path, action})
			data, _ := os.ReadFile(path)
			if !bytes.Equal(data, encoded) {
				t.Fatal("read-only command rewrote config")
			}
			if data, err := os.ReadFile(next); err != nil || string(data) != "pending" {
				t.Fatal("read-only command recovered transaction")
			}
			if data, err := os.ReadFile(journal); err != nil || string(data) != "malformed journal" {
				t.Fatal("read-only command touched journal")
			}
		})
	}
}

func TestManagementRejectsUnauthorizedAndMalformedRequests(t *testing.T) {
	for _, test := range []struct {
		name, request string
		admin         bool
	}{
		{"unauthorized", `{"version":1,"action":"enroll"}`, false},
		{"unknown verb", `{"version":1,"action":"delete"}`, true},
		{"extra path", `{"version":1,"action":"enroll","path":"/outside"}`, true},
		{"wrong protocol", `{"version":9,"action":"doctor"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()
			client.SetDeadline(time.Now().Add(2 * time.Second))
			m := &managementServer{path: filepath.Join(t.TempDir(), "must-not-be-read")}
			done := make(chan struct{})
			go func() { defer close(done); m.serve(context.Background(), server, test.admin) }()
			if _, err := io.WriteString(client, test.request+"\n"); err != nil {
				t.Fatal(err)
			}
			var response managementResponse
			if err := json.NewDecoder(client).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if !response.Done || response.Error == "" {
				t.Fatalf("unsafe request accepted: %+v", response)
			}
			<-done
		})
	}
}

func TestManagementCredentialMutationIsSerialized(t *testing.T) {
	m := &managementServer{path: filepath.Join(t.TempDir(), "not-read")}
	m.mutation.Lock()
	defer m.mutation.Unlock()
	server, client := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	go m.serve(context.Background(), server, true)
	json.NewEncoder(client).Encode(managementRequest{1, "rotate-credential"})
	var response managementResponse
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Error, "in progress") {
		t.Fatalf("concurrent mutation not rejected: %+v", response)
	}
}

func TestManagementDisconnectCancelsRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server, client := net.Pipe()
	m := &managementServer{path: filepath.Join(t.TempDir(), "unused")}
	done := make(chan struct{})
	go func() { defer close(done); m.serve(ctx, server, false) }()
	cancel()
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("management handler leaked after cancellation")
	}
}

func TestUpdateHandoffRejectsArbitraryTargets(t *testing.T) {
	for _, args := range [][]string{{"-target", "/outside", "-version", "1.2.3"}, {"-config", "/outside", "-version", "1.2.3"}, {"-version", "1.2.3", "extra"}} {
		if err := execute(append([]string{"apply-update"}, args...)); err == nil {
			t.Fatalf("unsafe handoff accepted: %q", args)
		}
	}
}

func TestSafeLeafRejectsWindowsRedirectionOnEveryPlatform(t *testing.T) {
	for _, name := range []string{"../outside", "..\\outside", "C:outside", "file:stream", "file.", "file ", "/absolute"} {
		if safeLeaf(name) {
			t.Fatalf("unsafe file name accepted: %q", name)
		}
	}
}

func TestNativeActionsRejectStateAndEnvironmentOverrides(t *testing.T) {
	t.Setenv("TELRAD_RELAY_SERVICE_ENROLL", "1")
	for _, args := range [][]string{{"auth", "--config", "/outside"}, {"update", "1.2.3", "/outside"}, {"start", "other.service"}, {"doctor"}, {"ready"}, {"version"}, {"status"}} {
		if err := validateNativeAction(args); err == nil {
			t.Fatalf("unsafe native action accepted: %q", args)
		}
	}
	if err := executeNativeAction([]string{"auth", "--config", "/outside"}, nil); err == nil {
		t.Fatal("invalid privileged action executed")
	}
}

func TestReadOnlyConfigRequiresExplicitMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	cfg := defaultConfig()
	cfg.SchemaVersion = 3
	data, _ := json.Marshal(cfg)
	os.WriteFile(path, data, 0600)
	if _, err := loadConfigMode(path, false); err == nil {
		t.Fatal("read-only config silently migrated")
	}
	if _, err := loadConfigMode(path, true); err != nil {
		t.Fatal(err)
	}
	var migrated config
	newData, _ := os.ReadFile(path)
	json.Unmarshal(newData, &migrated)
	if migrated.SchemaVersion != currentConfigSchemaVersion {
		t.Fatal("explicit migration did not run")
	}
	if _, err := safeReadFile(filepath.Join(filepath.Dir(path), "missing"), 1024); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error: %v", err)
	}
}

func TestManagedCommandPrivilegeMatrix(t *testing.T) {
	originalDistribution := distribution
	originalRequest := requestNativeAction
	t.Cleanup(func() { distribution = originalDistribution; requestNativeAction = originalRequest })
	for _, dist := range []string{"native", "docker"} {
		distribution = dist
		for _, action := range []string{"version", "status", "ready", "doctor", "update", "auth", "enroll", "rotate-credential", "start", "stop", "restart"} {
			if dist == "docker" && (action == "auth" || action == "enroll" || action == "rotate-credential" || action == "start" || action == "stop" || action == "restart") {
				continue
			}
			if dist == "native" && !nativeManagementEnabled(defaultConfigPath()) {
				continue
			}
			calls := 0
			requestNativeAction = func(args []string, _ io.Reader) error {
				calls++
				if len(args) != 1 || args[0] != action {
					t.Fatalf("incorrect privileged action %q", args)
				}
				return nil
			}
			// Managed read-only commands may report missing installation/service; none
			// may ask an administrator to make the diagnostic succeed.
			_ = execute([]string{action})
			want := 0
			if dist == "native" && (action == "auth" || action == "enroll" || action == "rotate-credential" || action == "start" || action == "stop" || action == "restart") {
				want = 1
			}
			if calls != want {
				t.Fatalf("%s %s: privileged calls %d, want %d", dist, action, calls, want)
			}
		}
	}
}

func TestUnpairedManagementStartsWithoutClinicalListenersAndStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	data, _ := json.Marshal(defaultConfig())
	os.WriteFile(path, data, 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runManagementListeners(ctx, path, []managementListener{{listener, false}}) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := readRuntimeStatus(path)
		if err == nil && state.State == "unpaired" {
			if state.IngestReady {
				t.Fatal("unpaired service opened clinical listeners")
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("management did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("management shutdown leaked")
	}
}

func TestSignedPayloadIsReverifiedAcrossPrivilegeBoundary(t *testing.T) {
	artifact := []byte("synthetic signed candidate")
	server, cfg, _ := signedUpdateServer(t, "2.0.0", artifact)
	defer server.Close()
	trust, err := loadUpdateTrust(cfg, filepath.Join(t.TempDir(), "relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	release, err := fetchUpdateRelease(context.Background(), trust, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	original := updateTransferMetadata{Manifest: release.Manifest, Platform: release.Platform, Size: int64(len(artifact))}
	if err := verifyApprovedPayload(trust, "2.0.0", original, artifact); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"artifact", "version", "channel", "platform", "size", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			encoded, _ := json.Marshal(original)
			var metadata updateTransferMetadata
			json.Unmarshal(encoded, &metadata)
			body := append([]byte(nil), artifact...)
			switch kind {
			case "artifact":
				body[0] ^= 1
			case "version":
				metadata.Manifest.Version = "3.0.0"
			case "channel":
				metadata.Manifest.Channel = "testing"
			case "platform":
				metadata.Platform = "wrong-platform"
			case "size":
				metadata.Size++
			case "metadata":
				metadata.Manifest.SourceRevision = strings.Repeat("a", 40)
			}
			if err := verifyApprovedPayload(trust, "2.0.0", metadata, body); err == nil {
				t.Fatal("tampered approved payload accepted")
			}
		})
	}
}

func TestDisconnectedPairingResumesAcceptedDrain(t *testing.T) {
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/relay/device-authorizations":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"requestId":"review","deviceSecret":%q,"verificationUri":"https://example.invalid/approve","expiresAt":%q,"intervalSeconds":3}`, strings.Repeat("s", 40), time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/review/token"):
			fmt.Fprintf(w, `{"pairingToken":%q}`, strings.Repeat("t", 40))
		case r.URL.Path == "/v1/relay/pairing-enrollments":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"relayId":"review-relay","credential":%q,"protocolVersion":1}`, testCredential('P'))
		default:
			t.Errorf("unexpected cloud path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cloud.Close()
	oldFactory := clientFactory
	clientFactory = func(*config) protocolClients { return protocolClients{secure: cloud.Client(), updates: cloud.Client()} }
	defer func() { clientFactory = oldFactory }()
	path := filepath.Join(t.TempDir(), "relay.json")
	cfg := defaultConfig()
	cfg.PairingURL = cloud.URL + "/v1/relay/pairing-enrollments"
	encoded, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	lifetime, end := context.WithCancel(context.Background())
	defer end()
	requestContext, cancelRequest := context.WithCancel(lifetime)
	defer cancelRequest()
	m := &managementServer{path: path, transitions: make(chan clinicalTransition), lifetime: lifetime}
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { defer close(done); m.serve(requestContext, server, true) }()
	if err := json.NewEncoder(client).Encode(managementRequest{1, "enroll"}); err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, client)
	var drain clinicalTransition
	select {
	case drain = <-m.transitions:
		if !drain.stop {
			t.Fatal("expected drain")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pairing never requested drain")
	}
	// The supervisor has accepted the stop, but active work has not drained yet.
	// Cancellation closes the IPC connection while the supervisor still owns it.
	cancelRequest()
	select {
	case <-done:
		t.Fatal("handler abandoned the accepted drain")
	case <-time.After(20 * time.Millisecond):
	}
	drain.result <- nil
	select {
	case resume := <-m.transitions:
		if resume.stop {
			t.Fatal("expected resume")
		}
		resume.result <- nil
		<-done
	case <-done:
		t.Fatal("pairing handler exited without scheduling resume after accepted drain")
	case <-time.After(time.Second):
		t.Fatal("cancellation did not complete or schedule recovery")
	}
}
