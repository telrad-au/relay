package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Installation targets are never read from the service's configuration or journals.
type managedPaths struct{ Config, Credential, Executable, Trust, Installation string }

func nativePaths() managedPaths {
	data, binaries := platformManagedDirectories()
	executable := "telrad"
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	return managedPaths{
		Config: filepath.Join(data, "relay.json"), Credential: filepath.Join(data, "relay-credential.json"),
		Executable: filepath.Join(binaries, executable), Trust: filepath.Join(binaries, "update-trust.json"),
		Installation: filepath.Join(binaries, "installation.json"),
	}
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func managedConfig(path string) bool {
	return distribution == "native" && samePath(path, nativePaths().Config)
}

func normalizeManagedCredential(cfg *config, path string) error {
	if !managedConfig(path) {
		return nil
	}
	if !samePath(absolute(filepath.Dir(path), cfg.CredentialPath), nativePaths().Credential) {
		return errors.New("managed credentialPath must be relay-credential.json; repair the installation before continuing")
	}
	cfg.CredentialPath = "relay-credential.json"
	return nil
}

func defaultConfigPath() string      { return nativePaths().Config }
func defaultUpdateTrustPath() string { return nativePaths().Trust }

func readProtectedFile(path string, limit int64) ([]byte, error) {
	d, err := openSafeDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if err := validateAdministratorHandle(d.file); err != nil {
		return nil, err
	}
	f, err := d.open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := validateAdministratorHandle(f); err != nil {
		return nil, err
	}
	return readBoundedFile(f, limit)
}

func validateManagedLayout() error {
	p := nativePaths()
	for _, path := range []string{filepath.Dir(p.Executable), filepath.Dir(filepath.Dir(p.Config))} {
		d, err := openSafeDirectory(path)
		if err != nil {
			return err
		}
		err = validateAdministratorHandle(d.file)
		d.Close()
		if err != nil {
			return err
		}
	}
	for _, path := range []string{p.Executable, p.Trust, p.Installation} {
		if _, err := readProtectedFile(path, 100*1024*1024); err != nil {
			return err
		}
	}
	d, err := openSafeDirectory(filepath.Dir(p.Config))
	if err != nil {
		return err
	}
	defer d.Close()
	for _, name := range []string{"relay.json", "relay-credential.json", "relay.json.pairing-transaction.json"} {
		if err := d.check(name); err != nil {
			return err
		}
	}
	return nil
}

func requireManagedExecutable() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if !samePath(executable, nativePaths().Executable) {
		return errors.New("privileged actions require the protected installed Relay executable")
	}
	_, err = readProtectedFile(executable, 100*1024*1024)
	return err
}
