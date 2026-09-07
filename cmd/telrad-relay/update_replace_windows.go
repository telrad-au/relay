//go:build windows

package main

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func activateExecutable(staged, target string) error {
	err := safeRename(staged, target)
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return err
	}
	// The operator's CLI may still map the old executable while waiting for this
	// transaction. Windows permits renaming that image, but not overwriting it.
	// Keep one fixed retired name; it is reclaimed once those callers have exited.
	retired := target + ".retired"
	if err := safeRemove(retired); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := safeRename(target, retired); err != nil {
		return err
	}
	if err := safeRename(staged, target); err != nil {
		return errors.Join(err, safeRename(retired, target))
	}
	return nil
}
