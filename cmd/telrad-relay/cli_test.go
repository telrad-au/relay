package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pairedTestConfig(directory string) *config {
	cfg := defaultConfig()
	cfg.configPath = filepath.Join(directory, "relay.json")
	cfg.CredentialPath = filepath.Join(directory, "relay-credential.json")
	cfg.RelayID = "opaque-relay"
	cfg.ControlURL = "wss://ingest.dev.app.telrad.com.au/v1/relay/control"
	cfg.DicomURL = "https://ingest.dev.app.telrad.com.au/v1/relay/ingest/dicom"
	cfg.HL7URL = "https://ingest.dev.app.telrad.com.au/v1/relay/ingest/hl7"
	return cfg
}

func TestBareAndAuthAlreadyEnrolledStartServiceWithConciseResult(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := pairedTestConfig(directory)
	if err := atomicWriteJSON(cfg.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteJSON(cfg.CredentialPath, credentialFile{
		SchemaVersion: credentialSchemaVersion,
		Credential:    testCredential('A'),
	}); err != nil {
		t.Fatal(err)
	}
	if !relayIsEnrolled(cfg) {
		_, err := readCredentialFile(cfg.CredentialPath, time.Now())
		t.Fatalf("test relay is not enrolled: %v", err)
	}

	originalStartService := startServiceForAuthentication
	startCalls := 0
	startServiceForAuthentication = func() error {
		startCalls++
		return nil
	}
	t.Cleanup(func() { startServiceForAuthentication = originalStartService })

	for _, args := range [][]string{
		{"--config", cfg.configPath},
		{"--config", cfg.configPath, "auth"},
	} {
		var authenticationErr error
		output := captureStandardOutput(t, func() {
			authenticationErr = execute(args)
		})
		if authenticationErr != nil {
			t.Fatal(authenticationErr)
		}
		for _, expected := range []string{
			"Telrad Relay is already authenticated and running.",
			"Run 'telrad status' for details.",
		} {
			if !strings.Contains(output, expected) {
				t.Fatalf("authentication output %q does not contain %q", output, expected)
			}
		}
	}
	if startCalls != 2 {
		t.Fatalf("service start calls = %d, want 2", startCalls)
	}
	if err := execute([]string{"--config", cfg.configPath, "setup"}); err == nil || !strings.Contains(err.Error(), `unknown command "setup"`) {
		t.Fatalf("removed setup command error = %v", err)
	}
}

func TestCommandContractUsesAuthInsteadOfSetup(t *testing.T) {
	output := captureStandardOutput(t, printHelp)
	if !strings.Contains(output, "telrad auth") {
		t.Fatalf("help output %q does not advertise telrad auth", output)
	}
	if strings.Contains(output, "telrad setup") {
		t.Fatalf("help output %q still advertises telrad setup", output)
	}
	if !commandRequiresAdministrator("auth", defaultConfigPath()) {
		t.Fatal("auth does not require administrator access for the managed configuration")
	}
	if !commandRequiresAdministrator("update", defaultConfigPath()) {
		t.Fatal("update does not require administrator access for the managed installation")
	}
	if commandRequiresAdministrator("setup", defaultConfigPath()) {
		t.Fatal("removed setup command still has command privileges")
	}
}

func captureStandardOutput(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })

	run()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = original
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestValidateConfigRequiresExactCommonOriginEndpoints(t *testing.T) {
	valid := pairedTestConfig(t.TempDir())
	if err := validateConfig(valid, "run"); err != nil {
		t.Fatal(err)
	}
	tests := []func(*config){
		func(cfg *config) { cfg.PairingURL += "?token=secret" },
		func(cfg *config) { cfg.PairingURL += "?" },
		func(cfg *config) { cfg.ControlURL = "wss://other.example/v1/relay/control" },
		func(cfg *config) { cfg.DicomURL = "http://ingest.dev.app.telrad.com.au/v1/relay/ingest/dicom" },
		func(cfg *config) { cfg.HL7URL += "/legacy" },
		func(cfg *config) {
			cfg.PairingURL = "https://user:pass@ingest.dev.app.telrad.com.au/v1/relay/pairing-enrollments"
		},
	}
	for index, mutate := range tests {
		copy := *valid
		mutate(&copy)
		if err := validateConfig(&copy, "run"); err == nil {
			t.Fatalf("invalid endpoint case %d was accepted", index)
		}
	}
}

func TestLoadConfigRejectsUnknownFieldsAndAppliesEnvironment(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "relay.json")
	cfg := defaultConfig()
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELRAD_RELAY_PAIRING_URL", "https://example.test/v1/relay/pairing-enrollments")
	t.Setenv("TELRAD_RELAY_REPORT_DESTINATION_HOST", "report.example.test")
	t.Setenv("TELRAD_RELAY_REPORT_DESTINATION_PORT", "12576")
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PairingURL != "https://example.test/v1/relay/pairing-enrollments" {
		t.Fatalf("pairing URL=%q", loaded.PairingURL)
	}
	if loaded.ReportHost != "report.example.test" || loaded.ReportPort != 12576 {
		t.Fatalf("report destination=%s:%d", loaded.ReportHost, loaded.ReportPort)
	}
	if loaded.CredentialPath != filepath.Join(directory, "relay-credential.json") {
		t.Fatalf("credential path=%q", loaded.CredentialPath)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":3,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestMigrateConfigHardCutsV2AndRemovesCertificateMaterial(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "relay.json")
	legacy := map[string]any{
		"schemaVersion": 2, "enrollmentUrl": "https://old.invalid/enroll", "controlUrl": "wss://old.invalid/control",
		"certificatePath": "relay.crt", "privateKeyPath": "relay.key", "caCertificatePath": "ca.crt",
		"listenAddress": "127.0.0.1", "dicomPort": 21112, "hl7Port": 12575, "reportHost": "127.0.0.2", "reportPort": 12576,
		"maxConnections": 20, "maxDicomConnections": 8, "maxHl7Connections": 12,
		"connectTimeoutSeconds": 7, "tlsHandshakeTimeoutSeconds": 9, "dicomIdleTimeoutSeconds": 60, "dicomLifetimeSeconds": 600,
		"hl7IdleTimeoutSeconds": 0, "hl7LifetimeSeconds": 0,
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relay.crt", "relay.key", "ca.crt", "relay.key.pairing-key.pem", "relay.key.pairing.csr.pem"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("obsolete"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateConfig(path, "", "", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchemaVersion != 3 || cfg.RelayID != "" || cfg.ControlURL != "" || cfg.DicomPort != 21112 || cfg.MaxConnections != 20 {
		t.Fatalf("migrated config=%#v", cfg)
	}
	encoded, _ := os.ReadFile(path)
	for _, obsolete := range []string{"certificatePath", "privateKeyPath", "caCertificatePath", "enrollmentUrl"} {
		if strings.Contains(string(encoded), obsolete) {
			t.Fatalf("migrated config retains %q", obsolete)
		}
	}
	for _, name := range []string{"relay.crt", "relay.key", "ca.crt", "relay.key.pairing-key.pem", "relay.key.pairing.csr.pem"} {
		if _, err := os.Stat(filepath.Join(directory, name)); !os.IsNotExist(err) {
			t.Fatalf("obsolete %s remains", name)
		}
	}
}

func TestDockerPairingTokenIsConsumedOnce(t *testing.T) {
	old := distribution
	distribution = "docker"
	t.Cleanup(func() { distribution = old })
	t.Setenv("TELRAD_RELAY_PAIRING_TOKEN", strings.Repeat("x", 40))
	value, err := consumeDockerPairingToken()
	if err != nil || len(value) != 40 {
		t.Fatalf("token length=%d error=%v", len(value), err)
	}
	zeroBytes(value)
	if _, exists := os.LookupEnv("TELRAD_RELAY_PAIRING_TOKEN"); exists {
		t.Fatal("pairing token remained in environment")
	}
}

func TestDockerPairingTokenIsUnsetEvenWhenAlreadyPairedCommandDoesNotUseIt(t *testing.T) {
	old := distribution
	distribution = "docker"
	t.Cleanup(func() { distribution = old })
	t.Setenv("TELRAD_RELAY_PAIRING_TOKEN", strings.Repeat("x", 40))
	if err := execute([]string{"version"}); err != nil {
		t.Fatal(err)
	}
	if _, exists := os.LookupEnv("TELRAD_RELAY_PAIRING_TOKEN"); exists {
		t.Fatal("unused Docker pairing token remained in the process environment")
	}
}

func TestExecuteHelpAndVersionNeedNoConfig(t *testing.T) {
	if err := execute([]string{"help"}); err != nil {
		t.Fatal(err)
	}
	if err := execute([]string{"version"}); err != nil {
		t.Fatal(err)
	}
}
