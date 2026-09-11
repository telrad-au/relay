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

const (
	credentialSchemaVersion          = 1
	credentialLifecycleSchemaVersion = 2
)

var (
	credentialPattern          = regexp.MustCompile(`^trr_v1_[A-Za-z0-9_-]{22}_[A-Za-z0-9_-]{43}$`)
	accessCredentialPattern    = regexp.MustCompile(`^trr_access_v2_[A-Za-z0-9_-]{22}_[A-Za-z0-9_-]{43}$`)
	renewableCredentialPattern = regexp.MustCompile(`^trr_renewable_v2_[A-Za-z0-9_-]{22}_[A-Za-z0-9_-]{43}$`)
	credentialOperationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
)

type credentialOperation struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type credentialFile struct {
	SchemaVersion      int        `json:"schemaVersion"`
	Credential         string     `json:"credential,omitempty"`
	PreviousCredential string     `json:"previousCredential,omitempty"`
	PreviousValidUntil *time.Time `json:"previousValidUntil,omitempty"`

	CredentialVersion   int                  `json:"credentialVersion,omitempty"`
	FamilyID            string               `json:"familyId,omitempty"`
	Generation          int                  `json:"generation,omitempty"`
	AccessCredential    string               `json:"accessCredential,omitempty"`
	AccessObtainedAt    time.Time            `json:"accessObtainedAt,omitempty"`
	AccessExpiresAt     time.Time            `json:"accessExpiresAt,omitempty"`
	RenewableCredential string               `json:"renewableCredential,omitempty"`
	RenewableExpiresAt  time.Time            `json:"renewableExpiresAt,omitempty"`
	RenewalURL          string               `json:"renewalUrl,omitempty"`
	PendingOperation    *credentialOperation `json:"pendingOperation,omitempty"`
}

func (record credentialFile) validate(now time.Time) error {
	switch record.SchemaVersion {
	case credentialSchemaVersion:
		if record.CredentialVersion != 0 || record.FamilyID != "" || record.Generation != 0 || record.AccessCredential != "" || !record.AccessObtainedAt.IsZero() || !record.AccessExpiresAt.IsZero() || record.RenewableCredential != "" || !record.RenewableExpiresAt.IsZero() || record.RenewalURL != "" {
			return errors.New("legacy credential contains lifecycle fields")
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
		if record.PendingOperation != nil && (record.PendingOperation.Kind != "migrate" || !credentialOperationPattern.MatchString(record.PendingOperation.ID)) {
			return errors.New("pending credential migration is invalid")
		}
		return nil
	case credentialLifecycleSchemaVersion:
		if record.Credential != "" || record.PreviousCredential != "" || record.PreviousValidUntil != nil {
			return errors.New("lifecycle credential contains legacy fields")
		}
		if record.CredentialVersion != 2 || !validOpaqueID(record.FamilyID) || record.Generation < 1 || !accessCredentialPattern.MatchString(record.AccessCredential) || !renewableCredentialPattern.MatchString(record.RenewableCredential) {
			return errors.New("lifecycle credential has an invalid format")
		}
		if record.AccessObtainedAt.IsZero() || record.AccessExpiresAt.IsZero() || record.RenewableExpiresAt.IsZero() || !record.AccessExpiresAt.After(record.AccessObtainedAt) || !record.RenewableExpiresAt.After(record.AccessExpiresAt) {
			return errors.New("lifecycle credential expiry is invalid")
		}
		if err := validateEndpointURL("renewalUrl", record.RenewalURL, "https", "/v1/relay/credentials/renew"); err != nil {
			return errors.New("lifecycle credential renewal URL is invalid")
		}
		if record.PendingOperation != nil && (record.PendingOperation.Kind != "renew" || !credentialOperationPattern.MatchString(record.PendingOperation.ID)) {
			return errors.New("pending credential renewal is invalid")
		}
		return nil
	default:
		return fmt.Errorf("credential schemaVersion %d is unsupported", record.SchemaVersion)
	}
}

func (record credentialFile) current() string {
	if record.SchemaVersion == credentialLifecycleSchemaVersion {
		return record.AccessCredential
	}
	return record.Credential
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
	data, err := safeReadFile(path, maxCloudResponseBytes)
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
	return provider.record.current()
}

func (provider *credentialProvider) Snapshot() credentialFile {
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	record := provider.record
	if record.PendingOperation != nil {
		operation := *record.PendingOperation
		record.PendingOperation = &operation
	}
	return record
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
	credentialLifecycleMutex.Lock()
	defer credentialLifecycleMutex.Unlock()
	fileLock, err := acquireCredentialOperationFileLock(provider.path)
	if err != nil {
		return err
	}
	defer fileLock.Close()
	provider.mu.Lock()
	defer provider.mu.Unlock()
	record, err := readCredentialFile(provider.path, now)
	if err != nil {
		return err
	}
	if record.PreviousValidUntil == nil || record.PreviousValidUntil.After(now) {
		provider.record = record
		return nil
	}
	record.PreviousCredential = ""
	record.PreviousValidUntil = nil
	if err := atomicWriteJSON(provider.path, record); err != nil {
		return err
	}
	provider.record = record
	return nil
}

func (provider *credentialProvider) Reload(now time.Time) (bool, error) {
	credentialLifecycleMutex.Lock()
	defer credentialLifecycleMutex.Unlock()
	fileLock, err := acquireCredentialOperationFileLock(provider.path)
	if err != nil {
		return false, err
	}
	defer fileLock.Close()
	provider.mu.Lock()
	defer provider.mu.Unlock()
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
	changed := record.current() != provider.record.current()
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
	if !samePath(filepath.Dir(configPath), filepath.Dir(cfg.CredentialPath)) || !safeLeaf(filepath.Base(cfg.CredentialPath)) {
		return errors.New("pairing credentials must be in the configuration directory")
	}
	if managedConfig(configPath) && !samePath(cfg.CredentialPath, nativePaths().Credential) {
		return errors.New("managed credential path is invalid; repair the installation")
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
	if err := validatePairingTransaction(configPath, transaction); err != nil {
		return err
	}
	if err := atomicWriteFile(transaction.ConfigNext, append(encodedConfig, '\n'), 0600); err != nil {
		return err
	}
	if err := atomicWriteFile(transaction.CredentialNext, encodedCredential, 0600); err != nil {
		_ = safeRemove(transaction.ConfigNext)
		return err
	}
	if err := atomicWriteJSON(pairingJournalPath(configPath), transaction); err != nil {
		return err
	}
	if err := validatePairingTransaction(configPath, transaction); err != nil {
		return err
	}
	return activatePairingTransaction(transaction)
}

func recoverPairingTransaction(configPath string) error {
	data, err := safeReadFile(pairingJournalPath(configPath), maxCloudResponseBytes)
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
	if err := validatePairingTransaction(configPath, transaction); err != nil {
		return err
	}
	if pairedFilesAreValid(transaction.ConfigPath, transaction.CredentialPath) {
		return finishPairingTransaction(transaction)
	}
	return rollbackPairingTransaction(transaction, nil)
}

// Recovery is constrained before even inspecting staged payloads. Legacy journals
// are compatible only when they describe the exact locally derived transaction.
func validatePairingTransaction(configPath string, t pairingTransaction) error {
	expectedCredential := filepath.Join(filepath.Dir(configPath), "relay-credential.json")
	if !managedConfig(configPath) {
		// Custom callers may choose a credential basename in the same directory.
		if samePath(filepath.Dir(t.CredentialPath), filepath.Dir(configPath)) && safeLeaf(filepath.Base(t.CredentialPath)) {
			expectedCredential = t.CredentialPath
		}
	}
	for _, pair := range [][2]string{{t.ConfigPath, configPath}, {t.CredentialPath, expectedCredential},
		{t.ConfigNext, configPath + ".next"}, {t.CredentialNext, expectedCredential + ".next"},
		{t.ConfigBackup, configPath + ".previous"}, {t.CredentialBack, expectedCredential + ".previous"}} {
		if !samePath(pair[0], pair[1]) || pair[0] != filepath.Clean(pair[0]) {
			return errors.New("pairing transaction paths are invalid; administrator repair is required")
		}
	}
	d, err := openSafeDirectory(filepath.Dir(configPath))
	if err != nil {
		return err
	}
	defer d.Close()
	for _, p := range []string{t.ConfigPath, t.CredentialPath, t.ConfigNext, t.CredentialNext, t.ConfigBackup, t.CredentialBack} {
		if err := d.check(filepath.Base(p)); err != nil {
			return err
		}
	}
	return nil
}

func removeOrphanedPairingStages(configPath string) error {
	var cleanupError error
	for _, path := range []string{configPath + ".next", filepath.Join(filepath.Dir(configPath), "relay-credential.json.next")} {
		if err := safeRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupError = errors.Join(cleanupError, err)
		}
	}
	return cleanupError
}

func activatePairingTransaction(transaction pairingTransaction) error {
	for _, path := range []string{transaction.ConfigBackup, transaction.CredentialBack} {
		if err := safeRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if transaction.HadConfig {
		if err := safeRename(transaction.ConfigPath, transaction.ConfigBackup); err != nil {
			return rollbackPairingTransaction(transaction, err)
		}
	}
	if transaction.HadCredential {
		if err := safeRename(transaction.CredentialPath, transaction.CredentialBack); err != nil {
			return rollbackPairingTransaction(transaction, err)
		}
	}
	if err := safeRename(transaction.CredentialNext, transaction.CredentialPath); err != nil {
		return rollbackPairingTransaction(transaction, fmt.Errorf("activate relay credential: %w", err))
	}
	if err := syncDirectory(filepath.Dir(transaction.CredentialPath)); err != nil {
		return rollbackPairingTransaction(transaction, err)
	}
	if err := safeRename(transaction.ConfigNext, transaction.ConfigPath); err != nil {
		return rollbackPairingTransaction(transaction, fmt.Errorf("activate relay configuration: %w", err))
	}
	if err := syncDirectory(filepath.Dir(transaction.ConfigPath)); err != nil {
		return rollbackPairingTransaction(transaction, err)
	}
	return finishPairingTransaction(transaction)
}

func finishPairingTransaction(transaction pairingTransaction) error {
	for _, path := range []string{transaction.ConfigBackup, transaction.CredentialBack, transaction.ConfigNext, transaction.CredentialNext} {
		if err := safeRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := safeRemove(pairingJournalPath(transaction.ConfigPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(transaction.ConfigPath))
}

func rollbackPairingTransaction(transaction pairingTransaction, cause error) error {
	var rollbackErr error
	restore := func(path, backup string, hadOriginal bool) {
		if hadOriginal {
			if _, err := os.Stat(backup); err == nil {
				_ = safeRemove(path)
				if err := safeRename(backup, path); err != nil {
					rollbackErr = errors.Join(rollbackErr, err)
				}
			}
		} else {
			if err := safeRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErr = errors.Join(rollbackErr, err)
			}
		}
	}
	restore(transaction.CredentialPath, transaction.CredentialBack, transaction.HadCredential)
	restore(transaction.ConfigPath, transaction.ConfigBackup, transaction.HadConfig)
	_ = safeRemove(transaction.CredentialNext)
	_ = safeRemove(transaction.ConfigNext)
	if rollbackErr == nil {
		_ = safeRemove(pairingJournalPath(transaction.ConfigPath))
	}
	return errors.Join(cause, rollbackErr)
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func pairedFilesAreValid(configPath, credentialPath string) bool {
	data, err := safeReadFile(configPath, maxCloudResponseBytes)
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

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	d, err := openDirectory(filepath.Dir(path), true)
	if err != nil {
		return err
	}
	d.Close()
	return safeAtomicWrite(path, data, mode)
}

func secureCredentialDirectory(directory string) error {
	d, err := openDirectory(directory, true)
	if err != nil {
		return err
	}
	defer d.Close()
	if runtime.GOOS != "windows" {
		return d.file.Chmod(0700)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := openSafeDirectory(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return syncDirectoryHandle(directory.file)
}
