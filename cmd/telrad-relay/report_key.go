package main

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const reportKeyFilename = "report-signing-key.json"

var reportKeyCreation sync.Mutex

// The existing protected credential directory belongs to this installation.
// Keep report authority separate from optional PACS retrieval configuration.
func reportKeyDirectory(cfg *config) string { return filepath.Dir(cfg.CredentialPath) }

func readReportSigningKey(cfg *config) (ed25519.PrivateKey, error) {
	return readLocalOrderKey(reportKeyDirectory(cfg), reportKeyFilename)
}

// Run before opening clinic listeners. Never overwrite an existing key, including
// a corrupt or unsafe file. Verification does not generate replacement authority.
func ensureReportSigningKey(cfg *config) error {
	reportKeyCreation.Lock()
	defer reportKeyCreation.Unlock()
	directory := reportKeyDirectory(cfg)
	d, err := openSafeDirectory(directory)
	if err != nil {
		return errors.New("report_key_directory_unavailable")
	}
	f, err := d.open(reportKeyFilename)
	d.Close()
	if err == nil {
		f.Close()
	} else if errors.Is(err, os.ErrNotExist) {
		if err := generateOrderKey(directory, reportKeyFilename); err != nil {
			return err
		}
	} else {
		return errors.New("report_key_unavailable")
	}
	key, err := readReportSigningKey(cfg)
	if err != nil {
		return err
	}
	zeroBytes(key)
	return nil
}
