//go:build windows

package main

// Windows does not support flushing a directory handle.
func isDirectorySyncUnsupported(error) bool { return true }
