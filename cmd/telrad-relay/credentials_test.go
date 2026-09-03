package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testCredential(suffix byte) string {
	return "trr_v1_" + strings.Repeat(string(suffix), 22) + "_" + strings.Repeat(string(suffix), 43)
}

func TestCredentialGrammarAndOverlap(t *testing.T) {
	now := time.Now()
	deadline := now.Add(time.Hour)
	valid := credentialFile{SchemaVersion: 1, Credential: testCredential('A'), PreviousCredential: testCredential('B'), PreviousValidUntil: &deadline}
	if err := valid.validate(now); err != nil {
		t.Fatal(err)
	}
	for _, record := range []credentialFile{
		{SchemaVersion: 2, Credential: testCredential('A')},
		{SchemaVersion: 1, Credential: "trr_v1_invalid"},
		{SchemaVersion: 1, Credential: testCredential('A'), PreviousCredential: testCredential('B')},
	} {
		if err := record.validate(now); err == nil {
			t.Fatalf("invalid credential accepted: %#v", record)
		}
	}
}

func TestCredentialProviderAdoptsRotationAndExpiresOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := commitCredential(path, credentialFile{SchemaVersion: 1, Credential: testCredential('A')}); err != nil {
		t.Fatal(err)
	}
	provider, err := newCredentialProvider(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(-time.Second)
	if err := atomicWriteJSON(path, credentialFile{SchemaVersion: 1, Credential: testCredential('B'), PreviousCredential: testCredential('A'), PreviousValidUntil: &deadline}); err != nil {
		t.Fatal(err)
	}
	changed, err := provider.Reload(time.Now())
	if err != nil || !changed || provider.Current() != testCredential('B') {
		t.Fatalf("reload changed=%t current=%q error=%v", changed, provider.Current(), err)
	}
	record, err := readCredentialFile(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if record.PreviousCredential != "" || record.PreviousValidUntil != nil {
		t.Fatal("expired overlap was not atomically removed")
	}
}

func TestCredentialWatcherExpiresOverlapAtDeadline(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "credential.json")
	deadline := time.Now().Add(80 * time.Millisecond)
	record := credentialFile{
		SchemaVersion: credentialSchemaVersion, Credential: testCredential('B'),
		PreviousCredential: testCredential('A'), PreviousValidUntil: &deadline,
	}
	if err := commitCredential(path, record); err != nil {
		t.Fatal(err)
	}
	provider, err := newCredentialProvider(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchCredentialFile(ctx, provider, make(chan struct{}, 1), newRuntimeStatus(filepath.Join(directory, "relay.json")))

	time.Sleep(250 * time.Millisecond)
	stored, err := readCredentialFile(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stored.PreviousCredential != "" || stored.PreviousValidUntil != nil {
		t.Fatal("deadline timer did not atomically remove the expired credential")
	}
}

func TestPairingCommitPermissionsAndRecovery(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	credentialPath := filepath.Join(directory, "relay-credential.json")
	oldConfig := []byte(`{"schemaVersion":2}`)
	oldCredential := []byte("old")
	if err := os.WriteFile(configPath, oldConfig, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialPath, oldCredential, 0600); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	cfg.configPath = configPath
	cfg.CredentialPath = credentialPath
	cfg.RelayID = "relay-1"
	cfg.ControlURL = "wss://ingest.dev.app.telrad.com.au/v1/relay/control"
	cfg.DicomURL = "https://ingest.dev.app.telrad.com.au/v1/relay/ingest/dicom"
	cfg.HL7URL = "https://ingest.dev.app.telrad.com.au/v1/relay/ingest/hl7"
	if err := commitPairing(configPath, cfg, credentialFile{SchemaVersion: 1, Credential: testCredential('A')}); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(configPath)
	if err != nil || loaded.RelayID != "relay-1" {
		t.Fatalf("load committed config: %#v %v", loaded, err)
	}
	if _, err := readCredentialFile(credentialPath, time.Now()); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(directory); info.Mode().Perm() != 0700 {
			t.Fatalf("directory mode=%o", info.Mode().Perm())
		}
		if info, _ := os.Stat(credentialPath); info.Mode().Perm() != 0600 {
			t.Fatalf("credential mode=%o", info.Mode().Perm())
		}
	}

	// Simulate a crash after both old files were backed up and only the new
	// credential was activated. Recovery must restore the complete old pair.
	transaction := pairingTransaction{ConfigPath: configPath, CredentialPath: credentialPath, ConfigNext: configPath + ".next", CredentialNext: credentialPath + ".next", ConfigBackup: configPath + ".previous", CredentialBack: credentialPath + ".previous", HadConfig: true, HadCredential: true}
	currentConfig, _ := os.ReadFile(configPath)
	currentCredential, _ := os.ReadFile(credentialPath)
	if err := os.Rename(configPath, transaction.ConfigBackup); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(credentialPath, transaction.CredentialBack); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transaction.ConfigNext, currentConfig, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialPath, currentCredential, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transaction.CredentialNext, currentCredential, 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteJSON(pairingJournalPath(configPath), transaction); err != nil {
		t.Fatal(err)
	}
	if err := recoverPairingTransaction(configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatal("config was not restored")
	}
	if _, err := os.Stat(credentialPath); err != nil {
		t.Fatal("credential was not restored")
	}
	if _, err := os.Stat(pairingJournalPath(configPath)); !os.IsNotExist(err) {
		t.Fatal("journal was not cleared")
	}
}

func TestCredentialErrorsNeverContainSecret(t *testing.T) {
	secret := testCredential('S')
	path := filepath.Join(t.TempDir(), "credential.json")
	data, _ := json.Marshal(map[string]any{"schemaVersion": 1, "credential": secret, "unexpected": secret})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readCredentialFile(path, time.Now())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
}
