package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"
)

const credentialSchemaVersion = 1

var credentialPattern = regexp.MustCompile(`^trr_v1_[A-Za-z0-9_-]{22}_[A-Za-z0-9_-]{43}$`)

type credentialFile struct {
	SchemaVersion      int        `json:"schemaVersion"`
	Credential         string     `json:"credential"`
	PreviousCredential string     `json:"previousCredential,omitempty"`
	PreviousValidUntil *time.Time `json:"previousValidUntil,omitempty"`
}

func (record credentialFile) validate(now time.Time) error {
	if record.SchemaVersion != credentialSchemaVersion {
		return fmt.Errorf("credential schemaVersion %d is unsupported", record.SchemaVersion)
	}
	if !credentialPattern.MatchString(record.Credential) {
		return errors.New("credential has an invalid format")
	}
	if record.PreviousCredential == "" && record.PreviousValidUntil != nil {
		return errors.New("previousValidUntil requires previousCredential")
	}
	if record.PreviousCredential != "" {
		if !credentialPattern.MatchString(record.PreviousCredential) || record.PreviousValidUntil == nil {
			return errors.New("previous credential overlap is invalid")
		}
	}
	return nil
}

func readCredentialFile(path string, now time.Time) (credentialFile, error) {
	if runtime.GOOS != "windows" {
		info, err := os.Lstat(path)
		if err != nil {
			return credentialFile{}, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return credentialFile{}, errors.New("credential file permissions are invalid")
		}
		directory, err := os.Stat(filepath.Dir(path))
		if err != nil || directory.Mode().Perm() != 0700 {
			return credentialFile{}, errors.New("credential directory permissions are invalid")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return credentialFile{}, err
	}
	if len(data) > maxCloudResponseBytes {
		return credentialFile{}, errors.New("credential file exceeds the size limit")
	}
	var record credentialFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return credentialFile{}, errors.New("decode relay credential file")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return credentialFile{}, errors.New("decode relay credential file")
	}
	if err := record.validate(now); err != nil {
		return credentialFile{}, err
	}
	return record, nil
}

type credentialProvider struct {
	mu         sync.RWMutex
	path       string
	record     credentialFile
	generation uint64
}

func newCredentialProvider(path string, now time.Time) (*credentialProvider, error) {
	record, err := readCredentialFile(path, now)
	if err != nil {
		return nil, err
	}
	if record.PreviousValidUntil != nil && !record.PreviousValidUntil.After(now) {
		record.PreviousCredential = ""
		record.PreviousValidUntil = nil
		if err := atomicWriteJSON(path, record); err != nil {
			return nil, err
		}
	}
	return &credentialProvider{path: path, record: record, generation: 1}, nil
}

func (provider *credentialProvider) Current() string {
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	return provider.record.Credential
}

func (provider *credentialProvider) Generation() uint64 {
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	return provider.generation
}

func (provider *credentialProvider) PreviousDeadline() (time.Time, bool) {
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	if provider.record.PreviousValidUntil == nil {
		return time.Time{}, false
	}
	return *provider.record.PreviousValidUntil, true
}

func (provider *credentialProvider) ExpirePrevious(now time.Time) error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.record.PreviousValidUntil == nil || provider.record.PreviousValidUntil.After(now) {
		return nil
	}
	record := provider.record
	record.PreviousCredential = ""
	record.PreviousValidUntil = nil
	if err := atomicWriteJSON(provider.path, record); err != nil {
		return err
	}
	provider.record = record
	return nil
}

func (provider *credentialProvider) Reload(now time.Time) (bool, error) {
	record, err := readCredentialFile(provider.path, now)
	if err != nil {
		return false, err
	}
	if record.PreviousValidUntil != nil && !record.PreviousValidUntil.After(now) {
		record.PreviousCredential = ""
		record.PreviousValidUntil = nil
		if err := atomicWriteJSON(provider.path, record); err != nil {
			return false, err
		}
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	changed := record.Credential != provider.record.Credential
	provider.record = record
	if changed {
		provider.generation++
	}
	return changed, nil
}

func commitCredential(path string, record credentialFile) error {
	if err := record.validate(time.Now()); err != nil {
		return err
	}
	if err := secureCredentialDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return atomicWriteJSON(path, record)
}

// Pairing is the only operation that changes configuration and credentials
// together. A durable journal makes recovery finish the fully staged pair.
type pairingTransaction struct {
	ConfigPath     string `json:"configPath"`
	CredentialPath string `json:"credentialPath"`
	ConfigNext     string `json:"configNext"`
	CredentialNext string `json:"credentialNext"`
	ConfigBackup   string `json:"configBackup"`
	CredentialBack string `json:"credentialBackup"`
	HadConfig      bool   `json:"hadConfig"`
	HadCredential  bool   `json:"hadCredential"`
}

func pairingJournalPath(configPath string) string { return configPath + ".pairing-transaction.json" }

func commitPairing(configPath string, cfg *config, record credentialFile) error {
	if err := record.validate(time.Now()); err != nil {
		return err
	}
	if filepath.Clean(configPath) == filepath.Clean(cfg.CredentialPath) {
		return errors.New("configuration and credential paths must be different")
	}
	if err := secureCredentialDirectory(filepath.Dir(cfg.CredentialPath)); err != nil {
		return err
	}
	persistedConfig := *cfg
	if cfg.credentialPathConfigured != "" {
		persistedConfig.CredentialPath = cfg.credentialPathConfigured
	}
	encodedConfig, err := json.MarshalIndent(&persistedConfig, "", "  ")
	if err != nil {
		return err
	}
	encodedCredential, err := json.Marshal(record)
	if err != nil {
		return err
	}
	transaction := pairingTransaction{
		ConfigPath:     configPath,
		CredentialPath: cfg.CredentialPath,
		ConfigNext:     configPath + ".next",
		CredentialNext: cfg.CredentialPath + ".next",
		ConfigBackup:   configPath + ".previous",
		CredentialBack: cfg.CredentialPath + ".previous",
	}
	transaction.HadConfig = regularFileExists(configPath)
	transaction.HadCredential = regularFileExists(cfg.CredentialPath)
	if err := atomicWriteFile(transaction.ConfigNext, append(encodedConfig, '\n'), 0600); err != nil {
		return err
	}
	if err := atomicWriteFile(transaction.CredentialNext, encodedCredential, 0600); err != nil {
		_ = os.Remove(transaction.ConfigNext)
		return err
	}
	if err := atomicWriteJSON(pairingJournalPath(configPath), transaction); err != nil {
		return err
	}
	return activatePairingTransaction(transaction)
}

func recoverPairingTransaction(configPath string) error {
	data, err := os.ReadFile(pairingJournalPath(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return removeOrphanedPairingStages(configPath)
	}
	if err != nil {
		return err
	}
	var transaction pairingTransaction
	if json.Unmarshal(data, &transaction) != nil || transaction.ConfigPath != configPath || transaction.CredentialPath == "" {
		return errors.New("pairing transaction journal is invalid; administrator repair is required")
	}
	if pairedFilesAreValid(transaction.ConfigPath, transaction.CredentialPath) {
		return finishPairingTransaction(transaction)
	}
	return rollbackPairingTransaction(transaction, nil)
}

func removeOrphanedPairingStages(configPath string) error {
	var cleanupError error
	if err := os.Remove(configPath + ".next"); err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanupError = errors.Join(cleanupError, err)
	}
	data, err := os.ReadFile(configPath)
	if err == nil {
		var paths struct {
			CredentialPath string `json:"credentialPath"`
		}
		if json.Unmarshal(data, &paths) == nil && paths.CredentialPath != "" {
			credentialPath := absolute(filepath.Dir(configPath), paths.CredentialPath)
			if err := os.Remove(credentialPath + ".next"); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupError = errors.Join(cleanupError, err)
			}
		}
	}
	return cleanupError
}

func activatePairingTransaction(transaction pairingTransaction) error {
	for _, path := range []string{transaction.ConfigBackup, transaction.CredentialBack} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if transaction.HadConfig {
		if err := os.Rename(transaction.ConfigPath, transaction.ConfigBackup); err != nil {
			return rollbackPairingTransaction(transaction, err)
		}
	}
	if transaction.HadCredential {
		if err := os.Rename(transaction.CredentialPath, transaction.CredentialBack); err != nil {
			return rollbackPairingTransaction(transaction, err)
		}
	}
	if err := os.Rename(transaction.CredentialNext, transaction.CredentialPath); err != nil {
		return rollbackPairingTransaction(transaction, fmt.Errorf("activate relay credential: %w", err))
	}
	if err := syncDirectory(filepath.Dir(transaction.CredentialPath)); err != nil {
		return rollbackPairingTransaction(transaction, err)
	}
	if err := os.Rename(transaction.ConfigNext, transaction.ConfigPath); err != nil {
		return rollbackPairingTransaction(transaction, fmt.Errorf("activate relay configuration: %w", err))
	}
	if err := syncDirectory(filepath.Dir(transaction.ConfigPath)); err != nil {
		return rollbackPairingTransaction(transaction, err)
	}
	return finishPairingTransaction(transaction)
}

func finishPairingTransaction(transaction pairingTransaction) error {
	for _, path := range []string{transaction.ConfigBackup, transaction.CredentialBack, transaction.ConfigNext, transaction.CredentialNext} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(pairingJournalPath(transaction.ConfigPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(transaction.ConfigPath))
}

func rollbackPairingTransaction(transaction pairingTransaction, cause error) error {
	var rollbackErr error
	restore := func(path, backup string, hadOriginal bool) {
		if hadOriginal {
			if _, err := os.Stat(backup); err == nil {
				_ = os.Remove(path)
				if err := os.Rename(backup, path); err != nil {
					rollbackErr = errors.Join(rollbackErr, err)
				}
			}
		} else {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, err)
			}
		}
	}
	restore(transaction.CredentialPath, transaction.CredentialBack, transaction.HadCredential)
	restore(transaction.ConfigPath, transaction.ConfigBackup, transaction.HadConfig)
	_ = os.Remove(transaction.CredentialNext)
	_ = os.Remove(transaction.ConfigNext)
	if rollbackErr == nil {
		_ = os.Remove(pairingJournalPath(transaction.ConfigPath))
	}
	return errors.Join(cause, rollbackErr)
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func pairedFilesAreValid(configPath, credentialPath string) bool {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if json.Unmarshal(data, &header) != nil || header.SchemaVersion != currentConfigSchemaVersion {
		return false
	}
	_, err = readCredentialFile(credentialPath, time.Now())
	return err == nil
}

func atomicWriteJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".telrad-write-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if err := preserveTargetOwner(temporary, path); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	return syncDirectory(directory)
}

func secureCredentialDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(directory, 0700)
	}
	return nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
