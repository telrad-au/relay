//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func validateManagedUpdateTrustOwnership(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("managed update trust must not be writable by its group or other users")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errors.New("managed update trust must be owned by root")
	}
	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("inspect managed update trust directory: %w", err)
	}
	directoryStat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !directoryInfo.IsDir() || !ok || directoryStat.Uid != 0 || directoryInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("managed update trust directory must be root-owned and not writable by its group or other users")
	}
	return nil
}
