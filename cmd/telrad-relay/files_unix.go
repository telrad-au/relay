//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

func isDirectorySyncUnsupported(error) bool { return false }

// matchDirectoryOwner gives a file written by root to the owner of its
// directory, so an operator using sudo does not leave a file the service
// account cannot read.
func matchDirectoryOwner(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(path, int(owner.Uid), int(owner.Gid))
}
