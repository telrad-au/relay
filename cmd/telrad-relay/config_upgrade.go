package main

import (
	"errors"
	"net/url"
)

// This upgrade changes only locally derived control transport configuration.
// In particular it never clears or replaces the enrolled credential.
func upgradePollingConfig(path string, cfg *config) error {
	if cfg.SchemaVersion == 4 {
		if cfg.Retrieval != nil || cfg.DisableDICOMListener {
			return errors.New("retrieval requires explicit schema-v5 provisioning")
		}
		cfg.SchemaVersion = currentConfigSchemaVersion
		command := "enroll"
		if cfg.RelayID != "" {
			command = "run"
		}
		if err := validateConfig(cfg, command); err != nil {
			return err
		}
		return atomicWriteJSON(path, cfg)
	}
	if cfg.SchemaVersion != 3 {
		return nil
	}
	if cfg.Retrieval != nil || cfg.DisableDICOMListener {
		return errors.New("retrieval requires explicit schema-v5 provisioning")
	}
	endpoints, err := deriveProtocolEndpoints(cfg.PairingURL)
	if err != nil {
		return err
	}
	paired := cfg.RelayID != "" || cfg.ControlURL != "" || cfg.DicomURL != "" || cfg.HL7URL != ""
	if paired {
		legacy, _ := url.Parse(endpoints.ControlURL)
		legacy.Scheme = "wss"
		if cfg.ControlURL != legacy.String() || cfg.DicomURL != endpoints.DicomURL || cfg.HL7URL != endpoints.HL7URL || !validOpaqueID(cfg.RelayID) {
			return errors.New("schema v3 endpoints do not match the approved pairing origin")
		}
		cfg.ControlURL = endpoints.ControlURL
	}
	cfg.SchemaVersion = currentConfigSchemaVersion
	applyConnectionDefaults(cfg)
	command := "enroll"
	if paired {
		command = "run"
	}
	if err := validateConfig(cfg, command); err != nil {
		return err
	}
	return atomicWriteJSON(path, cfg)
}
