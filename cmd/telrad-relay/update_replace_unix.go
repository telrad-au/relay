//go:build !windows

package main

func activateExecutable(staged, target string) error {
	return safeRename(staged, target)
}
