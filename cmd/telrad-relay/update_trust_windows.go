//go:build windows

package main

import "os"

func validateManagedUpdateTrustOwnership(_ string, _ os.FileInfo) error {
	return nil
}
