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
func atomicWriteFile(path string, data []byte, mode os.FileMode) (returnErr error) {
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
