//go:build windows

package main

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

func waitForProcessExit(processID int, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(processID))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, uint32(timeout/time.Millisecond))
	if err != nil {
		return err
	}
	if result != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process %d did not exit within %s", processID, timeout)
	}
	return nil
}
