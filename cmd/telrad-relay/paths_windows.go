//go:build windows

package main

import (
	"os"
	"path/filepath"
)

func programData() string {
	if value := os.Getenv("ProgramData"); value != "" {
		return value
	}
	return `C:\ProgramData`
}

func defaultConfigPath() string { return filepath.Join(programData(), "Telrad", "Relay", "relay.json") }
func defaultDataDir() string    { return filepath.Join(programData(), "Telrad", "Relay") }
