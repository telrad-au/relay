//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// A bind that fails because the running Relay already holds the address.
func listenerAddressInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
