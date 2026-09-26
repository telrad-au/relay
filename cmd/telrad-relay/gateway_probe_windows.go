//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Winsock reports an occupied address as WSAEADDRINUSE, which is not the
// invented syscall.EADDRINUSE value Go uses on Windows.
func listenerAddressInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }
