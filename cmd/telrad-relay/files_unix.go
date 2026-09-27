//go:build !windows

package main

func isDirectorySyncUnsupported(error) bool { return false }
