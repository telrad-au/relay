//go:build windows

package main

import "os"

// Windows does not support flushing a directory handle.
func isDirectorySyncUnsupported(error) bool { return true }

// matchDirectoryOwner is not needed on Windows: files inherit the data
// directory's access rules, which grant the service account.
func matchDirectoryOwner(string) error { return nil }

// copyOwner is not needed on Windows: the replacement inherits the data
// directory's access rules.
func copyOwner(*os.File, os.FileInfo) error { return nil }
