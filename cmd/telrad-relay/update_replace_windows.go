//go:build windows

package main

import "golang.org/x/sys/windows"

func activateExecutable(staged, target string) error {
	stagedPath, err := windows.UTF16PtrFromString(staged)
	if err != nil {
		return err
	}
	targetPath, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(stagedPath, targetPath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
