//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

func preserveTargetOwner(temporary *os.File, target string) error {
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return temporary.Chown(int(stat.Uid), int(stat.Gid))
}
