//go:build windows

package main

import "os"

func validateManagedUpdateTrustOwnership(path string, _ os.FileInfo) error {
	_, err := readProtectedFile(path, maxCloudResponseBytes)
	return err
}
