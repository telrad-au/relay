//go:build !windows

package main

import (
	"os"
	"syscall"
)

func launchStagedUpdate(staged string, args []string) error {
	return syscall.Exec(staged, append([]string{staged}, args...), os.Environ())
}
