//go:build !windows

package main

import "os"

func activateExecutable(staged, target string) error {
	return os.Rename(staged, target)
}
