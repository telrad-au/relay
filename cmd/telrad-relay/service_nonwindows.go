//go:build !windows

package main

func waitPlatformServiceControl([]string) error { return nil }

var runNonWindowsService = run

func runPlatformService(cfg *config, configPath string) error {
	return runNonWindowsService(cfg, configPath)
}
