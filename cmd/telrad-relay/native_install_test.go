//go:build !relay_container

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNativeInstallationCIPreparesProtectedPaths(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	remaining := string(data)
	for _, step := range []string{
		"name: Exercise native Linux installation and collect process coverage",
		"sudo chown root:root /usr/local/bin",
		"sudo chmod 0755 /usr/local/bin",
		"scripts/check-native-installation.sh",
		"name: Enforce combined Go coverage",
	} {
		_, after, found := strings.Cut(remaining, step)
		if !found {
			t.Fatalf("native CI missing ordered step %q", step)
		}
		remaining = after
	}
}

func isolatedInstallation(t *testing.T) (managedPaths, nativeInstallInput, nativeInstallOperations, *[]string) {
	t.Helper()
	root := t.TempDir()
	bin, state := filepath.Join(root, "bin"), filepath.Join(root, "state")
	for _, path := range []string{bin, state} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	paths := managedPaths{Config: filepath.Join(state, "relay.json"), Credential: filepath.Join(state, "relay-credential.json"), Executable: filepath.Join(bin, "telrad"), Trust: filepath.Join(bin, "update-trust.json"), Installation: filepath.Join(bin, "installation.json")}
	bundle := nativeInstallInput{Config: *defaultConfig(), Trust: updateTrust{SchemaVersion: 1, Channel: "stable", ManifestURL: "https://example.invalid/stable.json", PublicKey: strings.Repeat("A", 43) + "="}, Installation: json.RawMessage(`{"version":"2.0.0"}`)}
	var actions []string
	noop := func() error { return nil }
	ops := nativeInstallOperations{prepare: noop, secure: noop, validate: noop, reload: noop, running: func() (bool, error) { return true, nil }, start: func() error { actions = append(actions, "start-new"); return nil }, action: func(action string) error { actions = append(actions, action); return nil }, configure: func(string) error { return nil }}
	ops.readSnapshot = func(path string) ([]byte, error) { return safeReadFile(path, 100*1024*1024) }
	ops.snapshotSystem = func() (func() error, error) { return noop, nil }
	return paths, bundle, ops, &actions
}

func TestNativeInstallerPreservesConfigAndClearsInterruptedUpdate(t *testing.T) {
	paths, bundle, ops, actions := isolatedInstallation(t)
	previous := *defaultConfig()
	previous.ReportPort = 32576
	configBytes, _ := json.Marshal(previous)
	os.WriteFile(paths.Config, configBytes, 0600)
	os.WriteFile(paths.Executable, []byte("old binary"), 0755)
	keyPath := filepath.Join(filepath.Dir(paths.Credential), reportKeyFilename)
	if err := generateOrderKey(filepath.Dir(paths.Credential), reportKeyFilename); err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	credential := []byte("synthetic existing enrollment; installer must not reinterpret")
	os.WriteFile(paths.Credential, credential, 0600)
	for _, path := range []string{updateJournalPath(paths.Executable), paths.Executable + ".previous", paths.Executable + ".config.previous", paths.Executable + ".new", paths.Executable + ".new.manifest.json"} {
		os.WriteFile(path, []byte(`{"target":"/outside/must-not-be-used"}`), 0600)
	}
	if err := installNativeBundle(paths, bundle, []byte("new binary"), ops); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{keyPath: keyBytes, paths.Config: configBytes, paths.Credential: credential, paths.Executable: []byte("new binary"), paths.Installation: bundle.Installation} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s was not preserved/installed: %v", filepath.Base(path), err)
		}
	}
	if !reflect.DeepEqual(*actions, []string{"stop", "start-new"}) {
		t.Fatalf("service actions: %v", *actions)
	}
	entries, _ := os.ReadDir(filepath.Dir(paths.Executable))
	if len(entries) != 3 {
		t.Fatalf("stale protected recovery files: %v", entries)
	}
}

func TestNativeInstallerFailureRestoresLegacyEnrollmentAndService(t *testing.T) {
	paths, bundle, ops, actions := isolatedInstallation(t)
	legacy := []byte(`{"schemaVersion":2,"listenAddress":"127.0.0.1","dicomPort":21112,"hl7Port":12575,"reportHost":"127.0.0.1","reportPort":12576}`)
	originals := map[string][]byte{paths.Config: legacy, paths.Executable: []byte("old binary"), paths.Trust: []byte("old trust"), paths.Installation: []byte("old manifest")}
	for _, name := range legacyCredentialNames() {
		originals[filepath.Join(filepath.Dir(paths.Config), name)] = []byte("synthetic old enrollment " + name)
	}
	for path, data := range originals {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ops.start = func() error { *actions = append(*actions, "start-new"); return errors.New("injected startup failure") }
	if err := installNativeBundle(paths, bundle, []byte("new binary"), ops); err == nil || !strings.Contains(err.Error(), "injected startup failure") {
		t.Fatalf("failure lost: %v", err)
	}
	for path, want := range originals {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("rollback did not restore %s: %v", filepath.Base(path), err)
		}
	}
	if !reflect.DeepEqual(*actions, []string{"stop", "start-new", "stop", "start"}) {
		t.Fatalf("rollback service ordering: %v", *actions)
	}
}

func TestNativeInstallerRejectsUnsafeJournalBeforeMutation(t *testing.T) {
	paths, bundle, ops, actions := isolatedInstallation(t)
	journal := []byte(`{"configPath":"/outside","credentialPath":"/outside-secret"}`)
	os.WriteFile(pairingJournalPath(paths.Config), journal, 0600)
	ops.prepare = func() error { t.Fatal("prepared installation before checking unsafe journal"); return nil }
	if err := installNativeBundle(paths, bundle, []byte("new binary"), ops); err == nil {
		t.Fatal("unsafe journal accepted")
	}
	got, _ := os.ReadFile(pairingJournalPath(paths.Config))
	if !bytes.Equal(got, journal) || len(*actions) != 0 {
		t.Fatal("unsafe transaction caused a mutation")
	}
}

func TestNativeInstallerPreservesStoppedService(t *testing.T) {
	paths, bundle, ops, actions := isolatedInstallation(t)
	data, _ := json.Marshal(bundle.Config)
	os.WriteFile(paths.Config, data, 0600)
	os.WriteFile(paths.Executable, []byte("old binary"), 0755)
	ops.running = func() (bool, error) { return false, nil }
	if err := installNativeBundle(paths, bundle, []byte("new binary"), ops); err != nil {
		t.Fatal(err)
	}
	if len(*actions) != 0 {
		t.Fatalf("stopped service changed: %v", *actions)
	}
}

func TestNativeInstallerRestoresSystemChangesAfterLateFailure(t *testing.T) {
	paths, bundle, ops, _ := isolatedInstallation(t)
	data, _ := json.Marshal(bundle.Config)
	if err := os.WriteFile(paths.Config, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Executable, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}
	system := "previous service and links"
	before := system
	restored := false
	ops.snapshotSystem = func() (func() error, error) {
		return func() error {
			binary, err := os.ReadFile(paths.Executable)
			if err != nil || string(binary) != "old binary" {
				t.Fatal("system restored before executable")
			}
			system, restored = before, true
			return nil
		}, nil
	}
	ops.configure = func(string) error { system = "new service and links"; return nil }
	ops.start = func() error { return errors.New("late startup failure") }
	if err := installNativeBundle(paths, bundle, []byte("new binary"), ops); err == nil {
		t.Fatal("late failure ignored")
	}
	if !restored || system != before {
		t.Fatal("system changes survived rollback")
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(paths.Executable), ".installer-backup-*"))
	if err != nil || len(backups) != 0 {
		t.Fatal("successful rollback retained private snapshots")
	}
}
