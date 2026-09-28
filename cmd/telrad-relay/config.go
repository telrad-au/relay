package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const currentConfigSchemaVersion = 6

// defaultEnrolmentURL is the production enrolment endpoint. Development builds
// override it with -ldflags "-X main.defaultEnrolmentURL=...".
var defaultEnrolmentURL = "https://app.telrad.com.au/api/relay/enrolments"

type config struct {
	SchemaVersion         int    `json:"schemaVersion"`
	EnrolmentURL          string `json:"enrolmentUrl"`
	DataDir               string `json:"dataDir"`
	ListenAddress         string `json:"listenAddress"`
	DicomPort             int    `json:"dicomPort"`
	HL7Port               int    `json:"hl7Port"`
	ReportHost            string `json:"reportHost"`
	ReportPort            int    `json:"reportPort"`
	StatusAddress         string `json:"statusAddress"`
	MaxDicomConnections   int    `json:"maxDicomConnections"`
	MaxHL7Connections     int    `json:"maxHl7Connections"`
	HL7MaxBytes           int64  `json:"hl7MaxBytes"`
	HL7FrameSeconds       int    `json:"hl7FrameSeconds"`
	ConnectTimeoutSeconds int    `json:"connectTimeoutSeconds"`
	TelradAckSeconds      int    `json:"telradAckSeconds"`
	ReceiverAckSeconds    int    `json:"receiverAckSeconds"`
	IdleTimeoutSeconds    int    `json:"idleTimeoutSeconds"`

	// Test hooks. Never populated from configuration.
	rootCAs   certificatePool
	configDir string
}

func defaultConfig() *config {
	return &config{
		SchemaVersion: currentConfigSchemaVersion, EnrolmentURL: defaultEnrolmentURL, DataDir: defaultDataDir(),
		ListenAddress: "0.0.0.0", DicomPort: 11112, HL7Port: 2575, ReportPort: 2576, StatusAddress: "127.0.0.1:8425",
		MaxDicomConnections: 128, MaxHL7Connections: 128, HL7MaxBytes: 1024 * 1024, HL7FrameSeconds: 30,
		ConnectTimeoutSeconds: 10, TelradAckSeconds: 60, ReceiverAckSeconds: 30, IdleTimeoutSeconds: 900,
	}
}

// loadConfig reads the configuration file when it exists, then applies
// TELRAD_RELAY_* environment overrides. A missing file means defaults only.
func loadConfig(path string) (*config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if int64(len(data)) > 64*1024 {
			return nil, errors.New("relay configuration exceeds 64 KiB")
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(cfg); err != nil {
			return nil, fmt.Errorf("decode relay configuration: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, errors.New("decode relay configuration: multiple JSON values are not allowed")
		}
		cfg.configDir = filepath.Dir(path)
	}
	if err := applyEnvironment(cfg, os.LookupEnv); err != nil {
		return nil, err
	}
	if cfg.DataDir != "" && !filepath.IsAbs(cfg.DataDir) && cfg.configDir != "" {
		cfg.DataDir = filepath.Join(cfg.configDir, cfg.DataDir)
	}
	return cfg, nil
}

// applyEnvironment maps TELRAD_RELAY_<FIELD> to each configuration field, with
// FIELD the JSON name in upper snake case (hl7MaxBytes -> HL7_MAX_BYTES).
func applyEnvironment(cfg *config, lookup func(string) (string, bool)) error {
	targets := []struct {
		name string
		set  func(string) error
	}{
		{"enrolmentUrl", stringSetter(&cfg.EnrolmentURL)},
		{"dataDir", stringSetter(&cfg.DataDir)},
		{"listenAddress", stringSetter(&cfg.ListenAddress)},
		{"dicomPort", intSetter(&cfg.DicomPort)},
		{"hl7Port", intSetter(&cfg.HL7Port)},
		{"reportHost", stringSetter(&cfg.ReportHost)},
		{"reportPort", intSetter(&cfg.ReportPort)},
		{"statusAddress", stringSetter(&cfg.StatusAddress)},
		{"maxDicomConnections", intSetter(&cfg.MaxDicomConnections)},
		{"maxHl7Connections", intSetter(&cfg.MaxHL7Connections)},
		{"hl7MaxBytes", func(value string) error {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return errors.New("must be an integer")
			}
			cfg.HL7MaxBytes = parsed
			return nil
		}},
		{"hl7FrameSeconds", intSetter(&cfg.HL7FrameSeconds)},
		{"connectTimeoutSeconds", intSetter(&cfg.ConnectTimeoutSeconds)},
		{"telradAckSeconds", intSetter(&cfg.TelradAckSeconds)},
		{"receiverAckSeconds", intSetter(&cfg.ReceiverAckSeconds)},
		{"idleTimeoutSeconds", intSetter(&cfg.IdleTimeoutSeconds)},
	}
	for _, target := range targets {
		variable := environmentVariable(target.name)
		value, ok := lookup(variable)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if err := target.set(strings.TrimSpace(value)); err != nil {
			return fmt.Errorf("%s: %w", variable, err)
		}
	}
	return nil
}

func environmentVariable(jsonName string) string {
	var out strings.Builder
	out.WriteString("TELRAD_RELAY_")
	for index, character := range jsonName {
		if character >= 'A' && character <= 'Z' && index > 0 {
			out.WriteByte('_')
		}
		out.WriteString(strings.ToUpper(string(character)))
	}
	return out.String()
}

func stringSetter(target *string) func(string) error {
	return func(value string) error {
		*target = value
		return nil
	}
}

func intSetter(target *int) func(string) error {
	return func(value string) error {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return errors.New("must be an integer")
		}
		*target = parsed
		return nil
	}
}

func validateConfig(cfg *config) error {
	if cfg.SchemaVersion != currentConfigSchemaVersion {
		return fmt.Errorf("schemaVersion %d is unsupported; expected %d", cfg.SchemaVersion, currentConfigSchemaVersion)
	}
	if err := validateHTTPSURL("enrolmentUrl", cfg.EnrolmentURL); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.DataDir) == "" || !filepath.IsAbs(cfg.DataDir) {
		return errors.New("dataDir must be an absolute path")
	}
	if net.ParseIP(strings.TrimSpace(cfg.ListenAddress)) == nil {
		return errors.New("listenAddress must be an explicit IPv4 or IPv6 address")
	}
	for name, port := range map[string]int{"dicomPort": cfg.DicomPort, "hl7Port": cfg.HL7Port, "reportPort": cfg.ReportPort} {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%s must be an integer from 1 to 65535", name)
		}
	}
	if cfg.DicomPort == cfg.HL7Port {
		return errors.New("dicomPort and hl7Port must be different")
	}
	if strings.TrimSpace(cfg.ReportHost) == "" {
		return errors.New("reportHost is required")
	}
	host, port, err := net.SplitHostPort(cfg.StatusAddress)
	if err != nil {
		return errors.New("statusAddress must be host:port")
	}
	if address := net.ParseIP(host); address == nil || !address.IsLoopback() {
		return errors.New("statusAddress must use a loopback address")
	}
	if _, err := parsePort(port); err != nil {
		return fmt.Errorf("statusAddress port %w", err)
	}
	for name, value := range map[string]int{
		"maxDicomConnections": cfg.MaxDicomConnections, "maxHl7Connections": cfg.MaxHL7Connections,
		"hl7FrameSeconds": cfg.HL7FrameSeconds, "connectTimeoutSeconds": cfg.ConnectTimeoutSeconds,
		"telradAckSeconds": cfg.TelradAckSeconds, "receiverAckSeconds": cfg.ReceiverAckSeconds, "idleTimeoutSeconds": cfg.IdleTimeoutSeconds,
	} {
		if value < 1 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if cfg.HL7MaxBytes < 1 || cfg.HL7MaxBytes > 8*1024*1024 {
		return errors.New("hl7MaxBytes must be from 1 byte through 8 MiB")
	}
	return nil
}

func validateHTTPSURL(name, value string) error {
	parsed, err := parseURL(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s must be an absolute https URL without credentials", name)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain a query or fragment", name)
	}
	return nil
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("must be an integer from 1 to 65535")
	}
	return port, nil
}

func (cfg *config) dataPath(name string) string { return filepath.Join(cfg.DataDir, name) }
