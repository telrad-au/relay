package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateConfigRejectsUnsafeOperationalSettings(t *testing.T) {
	valid := pairedTestConfig(t.TempDir())
	if err := validateConfig(valid, "run"); err != nil {
		t.Fatal(err)
	}
	validUpdateKey := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	tests := []struct {
		name   string
		mutate func(*config)
	}{
		{"unsupported schema", func(cfg *config) { cfg.SchemaVersion-- }},
		{"hostname listen address", func(cfg *config) { cfg.ListenAddress = "localhost" }},
		{"zero DICOM port", func(cfg *config) { cfg.DicomPort = 0 }},
		{"oversize report port", func(cfg *config) { cfg.ReportPort = 65536 }},
		{"shared ingress port", func(cfg *config) { cfg.HL7Port = cfg.DicomPort }},
		{"zero global connections", func(cfg *config) { cfg.MaxConnections = 0 }},
		{"DICOM connections above global limit", func(cfg *config) { cfg.MaxDicomConnections = cfg.MaxConnections + 1 }},
		{"negative HL7 idle timeout", func(cfg *config) { cfg.HL7IdleTimeoutSeconds = -1 }},
		{"zero HL7 message limit", func(cfg *config) { cfg.HL7MaxBytes = 0 }},
		{"oversize HL7 message limit", func(cfg *config) { cfg.HL7MaxBytes = 8*1024*1024 + 1 }},
		{"DICOM idle beyond lifetime", func(cfg *config) { cfg.DicomIdleTimeoutSeconds = cfg.DicomLifetimeSeconds + 1 }},
		{"HL7 idle beyond lifetime", func(cfg *config) { cfg.HL7IdleTimeoutSeconds, cfg.HL7LifetimeSeconds = 2, 1 }},
		{"blank report host", func(cfg *config) { cfg.ReportHost = " " }},
		{"blank credential path", func(cfg *config) { cfg.CredentialPath = " " }},
		{"credential overwrites config", func(cfg *config) { cfg.CredentialPath = cfg.configPath }},
		{"invalid relay ID", func(cfg *config) { cfg.RelayID = "relay\nsecret" }},
		{"update URL without key", func(cfg *config) { cfg.UpdateManifestURL = "https://example.test/stable.json" }},
		{"update key without URL", func(cfg *config) { cfg.UpdatePublicKey = validUpdateKey }},
		{"insecure update URL", func(cfg *config) {
			cfg.UpdateManifestURL = "http://example.test/stable.json"
			cfg.UpdatePublicKey = validUpdateKey
		}},
		{"invalid update key", func(cfg *config) {
			cfg.UpdateManifestURL = "https://example.test/stable.json"
			cfg.UpdatePublicKey = "not-a-key"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := *valid
			test.mutate(&candidate)
			if err := validateConfig(&candidate, "run"); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestValidateConfigEnrollmentDoesNotRequireIssuedIdentity(t *testing.T) {
	cfg := pairedTestConfig(t.TempDir())
	cfg.RelayID = ""
	cfg.ControlURL = ""
	cfg.DicomURL = ""
	cfg.HL7URL = ""
	if err := validateConfig(cfg, "enroll"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigRejectsInvalidReportDestinationPortEnvironment(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "relay.json")
	data, err := json.Marshal(defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0", "65536", "not-a-port"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TELRAD_RELAY_REPORT_DESTINATION_PORT", value)
			_, err := loadConfig(path)
			if err == nil || !strings.Contains(err.Error(), "TELRAD_RELAY_REPORT_DESTINATION_PORT") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsMultipleJSONValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":3} {"schemaVersion":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("error = %v", err)
	}
}

func TestCustomUpdateTrustRequiresCompleteSecureConfiguration(t *testing.T) {
	validKey := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	valid := defaultConfig()
	valid.UpdateManifestURL = "https://example.test/stable.json"
	valid.UpdatePublicKey = validKey
	configPath := filepath.Join(t.TempDir(), "relay.json")
	trust, err := loadUpdateTrust(valid, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if trust.Channel != stableUpdateChannel || trust.ManifestURL != valid.UpdateManifestURL || trust.PublicKey != validKey {
		t.Fatalf("unexpected trust: %+v", trust)
	}

	tests := []struct {
		name   string
		mutate func(*config)
	}{
		{"missing URL", func(cfg *config) { cfg.UpdateManifestURL = "" }},
		{"missing key", func(cfg *config) { cfg.UpdatePublicKey = "" }},
		{"HTTP URL", func(cfg *config) { cfg.UpdateManifestURL = "http://example.test/stable.json" }},
		{"URL credentials", func(cfg *config) { cfg.UpdateManifestURL = "https://user:pass@example.test/stable.json" }},
		{"URL query", func(cfg *config) { cfg.UpdateManifestURL += "?channel=stable" }},
		{"invalid key", func(cfg *config) { cfg.UpdatePublicKey = "not-a-key" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := *valid
			test.mutate(&candidate)
			if _, err := loadUpdateTrust(&candidate, configPath); err == nil {
				t.Fatal("invalid update trust was accepted")
			}
		})
	}
}

func TestManagedUpdateTrustRejectsMissingAndNonRegularFiles(t *testing.T) {
	directory := t.TempDir()
	if err := validateManagedUpdateTrust(filepath.Join(directory, "missing.json")); err == nil {
		t.Fatal("missing trust file was accepted")
	}
	if err := validateManagedUpdateTrust(directory); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v", err)
	}
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "trust.json")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := validateManagedUpdateTrust(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestComposeUsesReportDestinationEnvironmentContract(t *testing.T) {
	for _, relativePath := range []string{"../../packaging/compose.yml", "../../packaging/compose.env.example"} {
		content, err := os.ReadFile(relativePath)
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		for _, required := range []string{"TELRAD_RELAY_REPORT_DESTINATION_HOST", "TELRAD_RELAY_REPORT_DESTINATION_PORT"} {
			if !strings.Contains(text, required) {
				t.Fatalf("%s does not contain %s", relativePath, required)
			}
		}
		for _, legacy := range []string{"TELRAD_RELAY_REPORT_HOST", "TELRAD_RELAY_REPORT_PORT"} {
			if strings.Contains(text, legacy) {
				t.Fatalf("%s still contains legacy variable %s", relativePath, legacy)
			}
		}
	}
}

func TestContainerPairingEndpointSourceAndRuntimeContract(t *testing.T) {
	const sourceDefault = "https://ingest.app.telrad.com.au/v1/relay/pairing-enrollments"
	const runtimeOverride = "https://development.example.test/v1/relay/pairing-enrollments"

	packaged, err := os.ReadFile("../../packaging/docker-relay.json")
	if err != nil {
		t.Fatal(err)
	}
	var packagedConfig config
	if err := json.Unmarshal(packaged, &packagedConfig); err != nil {
		t.Fatal(err)
	}
	if packagedConfig.PairingURL != sourceDefault {
		t.Fatalf("source-controlled container pairing URL = %q", packagedConfig.PairingURL)
	}
	if strings.Contains(string(packaged), previewPairingURL) {
		t.Fatal("container configuration still embeds the development pairing endpoint")
	}

	contracts := map[string][]string{
		"../../Dockerfile": {
			"install -m 0600 packaging/docker-relay.json /image-root/var/lib/telrad-relay/relay.json",
		},
		"../../.github/workflows/publish-release.yml": {
			`[[ "$ENROLLMENT_URL" == "$container_pairing_url" ]] || {`,
		},
		"../../packaging/compose.yml": {
			"TELRAD_RELAY_PAIRING_URL: ${TELRAD_RELAY_PAIRING_URL:-}",
		},
	}
	for path, required := range contracts {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range required {
			if !strings.Contains(string(content), value) {
				t.Fatalf("%s does not contain %q", path, value)
			}
		}
	}
	for _, path := range []string{"../../Dockerfile", "../../.github/workflows/publish-prerelease.yml"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "RELAY_CONTAINER_DEFAULT_PAIRING_URL") {
			t.Fatalf("%s still permits build-time pairing URL injection", path)
		}
	}

	configPath := filepath.Join(t.TempDir(), "relay.json")
	if err := os.WriteFile(configPath, packaged, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELRAD_RELAY_PAIRING_URL", runtimeOverride)
	loaded, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PairingURL != runtimeOverride {
		t.Fatalf("runtime pairing override = %q", loaded.PairingURL)
	}
}
