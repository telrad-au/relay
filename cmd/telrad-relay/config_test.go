package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDefaultsFileAndEnvironment(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "relay.json")
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("missing file must load defaults: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":6,"reportHost":"ris.local","dicomPort":4242,"dataDir":"state"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELRAD_RELAY_HL7_PORT", "2600")
	t.Setenv("TELRAD_RELAY_HL7_MAX_BYTES", "2048")
	t.Setenv("TELRAD_RELAY_REPORT_HOST", " ris2.local ")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DicomPort != 4242 || cfg.HL7Port != 2600 || cfg.HL7MaxBytes != 2048 || cfg.ReportHost != "ris2.local" {
		t.Fatalf("config=%+v", cfg)
	}
	if cfg.DataDir != filepath.Join(directory, "state") {
		t.Fatalf("relative dataDir=%q", cfg.DataDir)
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELRAD_RELAY_DICOM_PORT", "abc")
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "TELRAD_RELAY_DICOM_PORT") {
		t.Fatalf("bad environment value accepted: %v", err)
	}
}

func TestLoadConfigRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	for name, body := range map[string]string{
		"unknown field": `{"schemaVersion":6,"pairingUrl":"https://x"}`,
		"two values":    `{"schemaVersion":6}{}`,
		"not json":      `nope`,
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestEnvironmentVariableNames(t *testing.T) {
	for jsonName, expected := range map[string]string{
		"enrolmentUrl": "TELRAD_RELAY_ENROLMENT_URL", "hl7MaxBytes": "TELRAD_RELAY_HL7_MAX_BYTES",
		"maxHl7Connections": "TELRAD_RELAY_MAX_HL7_CONNECTIONS", "dicomPort": "TELRAD_RELAY_DICOM_PORT",
		"hl7Port": "TELRAD_RELAY_HL7_PORT", "hl7FrameSeconds": "TELRAD_RELAY_HL7_FRAME_SECONDS",
	} {
		if got := environmentVariable(jsonName); got != expected {
			t.Fatalf("%s -> %s, want %s", jsonName, got, expected)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	valid := func() *config {
		cfg := defaultConfig()
		cfg.ReportHost = "ris.local"
		return cfg
	}
	if err := validateConfig(valid()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*config){
		"schema":          func(c *config) { c.SchemaVersion = 5 },
		"http enrolment":  func(c *config) { c.EnrolmentURL = "http://example.invalid/e" },
		"enrolment query": func(c *config) { c.EnrolmentURL = "https://example.invalid/e?x=1" },
		"relative data":   func(c *config) { c.DataDir = "relative" },
		"listen name":     func(c *config) { c.ListenAddress = "localhost" },
		"same ports":      func(c *config) { c.HL7Port = c.DicomPort },
		"no report host":  func(c *config) { c.ReportHost = " " },
		"public status":   func(c *config) { c.StatusAddress = "0.0.0.0:8425" },
		"status no port":  func(c *config) { c.StatusAddress = "127.0.0.1" },
		"zero limit":      func(c *config) { c.MaxHL7Connections = 0 },
		"huge frame":      func(c *config) { c.HL7MaxBytes = 9 * 1024 * 1024 },
	}
	for name, mutate := range cases {
		cfg := valid()
		mutate(cfg)
		if err := validateConfig(cfg); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
