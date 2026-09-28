package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// atomicWriteFile writes data to a temporary file beside path, syncs it and
// renames it into place. Files are created for the service account only.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	return writeFileAtomically(path, data, mode, nil)
}

// replaceFileAtomically writes data over the existing file at path the same
// way, keeping the file's permission bits and, when run as root, its owner.
func replaceFileAtomically(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return writeFileAtomically(path, data, info.Mode().Perm(), func(file *os.File) error {
		// The creation mode is reduced by the umask; set it exactly.
		if err := file.Chmod(info.Mode().Perm()); err != nil {
			return err
		}
		return copyOwner(file, info)
	})
}

func writeFileAtomically(path string, data []byte, mode os.FileMode, prepare func(*os.File) error) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(path), ".telrad-write-"+hex.EncodeToString(suffix))
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			_ = os.Remove(temp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if prepare != nil {
		if err := prepare(file); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) && !isDirectorySyncUnsupported(err) {
		return err
	}
	return nil
}
