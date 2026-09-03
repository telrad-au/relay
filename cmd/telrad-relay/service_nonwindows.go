//go:build !windows

package main

var runNonWindowsService = run

func runPlatformService(cfg *config, configPath string) error {
	return runNonWindowsService(cfg, configPath)
}
