//go:build windows && !relay_container

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func prepareNativeDirectory(path string, serviceWritable bool) error {
	parent, err := openSafeDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	// Set owner and protected, inheritable ACLs as part of creation. Mkdir's
	// Unix permission bits do not prevent Windows from inheriting public writes.
	sd, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	directory, err := openRelativeWindows(parent.file, filepath.Base(path), windows.FILE_GENERIC_READ, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE, sd)
	existing := errors.Is(err, os.ErrExist)
	if existing {
		// Never replace or repair a pre-existing directory here, including links.
		directory, err = openRelativeDirectory(parent.file, filepath.Base(path))
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	// Only existing service state is intentionally writable by the service.
	if !existing || !serviceWritable {
		if err := validateAdministratorHandle(directory); err != nil {
			return &os.PathError{Op: "validate installation directory", Path: path, Err: err}
		}
	}
	return nil
}
