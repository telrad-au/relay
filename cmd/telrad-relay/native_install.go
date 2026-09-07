//go:build !relay_container

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Input comes from the reviewed installer bundle. It contains contents only;
// neither the bundle nor existing service state can supply filesystem targets.
type nativeInstallInput struct {
	Config              config          `json:"config"`
	Trust               updateTrust     `json:"trust"`
	Installation        json.RawMessage `json:"installation"`
	ClinicRemoteAddress string          `json:"clinicRemoteAddress,omitempty"`
}

func installNative(input io.Reader) error {
	if !platformAdministrator() {
		return errors.New("installation requires administrator authorization")
	}
	var bundle nativeInstallInput
	if err := decodeBoundedJSON(input, 1024*1024, &bundle); err != nil {
		return errors.New("invalid native installation input")
	}
	paths := nativePaths()
	if err := normalizeManagedCredential(&bundle.Config, paths.Config); err != nil {
		return err
	}
	if err := validateConfig(&bundle.Config, "enroll"); err != nil {
		return err
	}
	if err := validateUpdateTrust(bundle.Trust); err != nil {
		return err
	}
	var manifest map[string]any
	if json.Unmarshal(bundle.Installation, &manifest) != nil || manifest == nil {
		return errors.New("invalid installation manifest")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err := safeReadFile(self, 100*1024*1024)
	if err != nil {
		return err
	}
	return installNativeBundle(paths, bundle, binary, nativeInstallOperations{
		prepare: prepareNativeInstallation, running: nativeServiceRunning,
		action: serviceAction, start: func() error {
			if err := enableAndStartService(); err != nil {
				return err
			}
			return waitForManagement(context.Background())
		}, secure: secureNativeState,
		configure: configureNativeService, validate: validateManagedLayout,
		serviceFiles: nativeServiceFiles(), reload: reloadNativeService,
		snapshotSystem: snapshotNativeSystem,
		readSnapshot: func(path string) ([]byte, error) {
			if samePath(filepath.Dir(path), filepath.Dir(paths.Config)) {
				return safeReadFile(path, 100*1024*1024)
			}
			return readProtectedFile(path, 100*1024*1024)
		},
	})
}

// Only installNative supplies these paths and platform operations in production.
// Keeping the transaction separate also lets tests force a real mid-install
// failure and verify restoration without modifying the host installation.
type nativeInstallOperations struct {
	prepare, start, secure, validate, reload func() error
	running                                  func() (bool, error)
	action, configure                        func(string) error
	serviceFiles                             []string
	readSnapshot                             func(string) ([]byte, error)
	snapshotSystem                           func() (func() error, error)
}

func installNativeBundle(paths managedPaths, bundle nativeInstallInput, binary []byte, ops nativeInstallOperations) (returnErr error) {
	if data, err := safeReadFile(pairingJournalPath(paths.Config), maxCloudResponseBytes); err == nil {
		var transaction pairingTransaction
		if json.Unmarshal(data, &transaction) != nil {
			return errors.New("invalid pairing transaction; administrator repair is required")
		}
		if err := validatePairingTransaction(paths.Config, transaction); err != nil {
			return err
		}
		return errors.New("pending pairing transaction; let the existing service recover before reinstalling")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	hadInstallation := regularFileExists(paths.Executable)
	if err := ops.prepare(); err != nil {
		return err
	}
	restoreSystem, err := ops.snapshotSystem()
	if err != nil {
		return err
	}
	wasRunning, err := ops.running()
	if err != nil {
		return err
	}
	if wasRunning {
		if err := ops.action("stop"); err != nil {
			return err
		}
	}

	mutationsStarted := false
	defer func() {
		if returnErr != nil && wasRunning && !mutationsStarted {
			returnErr = errors.Join(returnErr, ops.action("start"))
		}
	}()
	// All snapshots live in administrator-owned storage and are copied by held
	// directory handles. Never place a rollback snapshot beside writable config.
	targets := []string{paths.Executable, paths.Trust, paths.Installation, paths.Config}
	targets = append(targets, ops.serviceFiles...)
	// A v2 migration removes only these known leaves. Snapshot them so failure
	// can restore the previous configuration together with its enrollment.
	if existing, err := safeReadFile(paths.Config, maxCloudResponseBytes); err == nil {
		var header struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if json.Unmarshal(existing, &header) == nil && header.SchemaVersion == 2 {
			for _, name := range legacyCredentialNames() {
				targets = append(targets, filepath.Join(filepath.Dir(paths.Config), name))
			}
		}
	}
	type snapshot struct {
		target, backup string
		exists         bool
	}
	var snapshots []snapshot
	for i, target := range targets {
		backup := filepath.Join(filepath.Dir(paths.Executable), fmt.Sprintf(".installer-backup-%d", i))
		data, err := ops.readSnapshot(target)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if err := safeAtomicWrite(backup, data, 0600); err != nil {
				return err
			}
		}
		snapshots = append(snapshots, snapshot{target, backup, err == nil})
	}
	committed := false
	defer func() {
		cleanup := committed
		if !committed {
			var rollbackErr error
			// Startup may have partially succeeded before reporting failure.
			rollbackErr = errors.Join(rollbackErr, ops.action("stop"))
			for _, s := range snapshots {
				if s.exists {
					data, err := safeReadFile(s.backup, 100*1024*1024)
					if err == nil {
						mode := os.FileMode(0644)
						if samePath(s.target, paths.Executable) {
							mode = 0755
						}
						if samePath(filepath.Dir(s.target), filepath.Dir(paths.Config)) {
							mode = 0600
						}
						err = safeAtomicWrite(s.target, data, mode)
					}
					rollbackErr = errors.Join(rollbackErr, err)
				} else {
					err := safeRemove(s.target)
					if !errors.Is(err, os.ErrNotExist) {
						rollbackErr = errors.Join(rollbackErr, err)
					}
				}
			}
			if regularFileExists(paths.Config) {
				rollbackErr = errors.Join(rollbackErr, ops.secure())
			}
			rollbackErr = errors.Join(rollbackErr, restoreSystem())
			rollbackErr = errors.Join(rollbackErr, ops.reload())
			if wasRunning && rollbackErr == nil {
				rollbackErr = ops.action("start")
			}
			returnErr = errors.Join(returnErr, rollbackErr)
			cleanup = rollbackErr == nil
		}
		if cleanup {
			for _, s := range snapshots {
				_ = safeRemove(s.backup)
			}
		}
	}()
	mutationsStarted = true
	if err := replaceExecutable(paths.Executable, binary); err != nil {
		return err
	}
	trust, _ := json.Marshal(bundle.Trust)
	if err := safeAtomicWrite(paths.Trust, trust, 0644); err != nil {
		return err
	}
	if err := safeAtomicWrite(paths.Installation, bundle.Installation, 0644); err != nil {
		return err
	}
	existing, err := safeReadFile(paths.Config, maxCloudResponseBytes)
	if errors.Is(err, os.ErrNotExist) {
		data, _ := json.Marshal(bundle.Config)
		if err := safeAtomicWrite(paths.Config, data, 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		var header struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if json.Unmarshal(existing, &header) != nil {
			return errors.New("installed configuration is invalid")
		}
		switch header.SchemaVersion {
		case 2:
			if err := migrateConfig(paths.Config, bundle.Config.PairingURL, bundle.Trust.ManifestURL, bundle.Trust.PublicKey); err != nil {
				return err
			}
		case 3, 4:
			if _, err := loadConfig(paths.Config); err != nil {
				return err
			}
		default:
			return errors.New("installed configuration schema is unsupported")
		}
	}
	if err := ops.secure(); err != nil {
		return err
	}
	if err := ops.configure(bundle.ClinicRemoteAddress); err != nil {
		return err
	}
	if err := ops.validate(); err != nil {
		return err
	}
	if !hadInstallation || wasRunning {
		if err := ops.start(); err != nil {
			return err
		}
	}
	committed = true
	// Reviewed reinstallation is the recovery boundary for interrupted updates.
	// Never interpret the old journal or execute its nominated paths.
	for _, path := range []string{updateJournalPath(paths.Executable), paths.Executable + ".previous", paths.Executable + ".config.previous", paths.Executable + ".new", paths.Executable + ".new.manifest.json"} {
		if err := safeRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("installation repaired but update cleanup failed: %w", err)
		}
	}
	// Old service-writable installation.json is deliberately ignored, not trusted
	// as migration input. The authoritative copy is now beside the executable.
	fmt.Printf("Telrad Relay %s installed.\n", version)
	if regularFileExists(paths.Credential) {
		fmt.Println("Existing authentication preserved.")
	} else if hadInstallation {
		fmt.Println("Existing configuration preserved.")
	}
	if wasRunning {
		fmt.Println("Service restarted successfully.\nRelay is running.")
	}
	if !hadInstallation {
		fmt.Println("Local management is running. Clinical listeners open after pairing.")
	}
	fmt.Println("Run 'telrad' to authenticate this host and start the service.")
	return nil
}
