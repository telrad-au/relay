package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	linuxServiceName   = "telrad-relay.service"
	windowsServiceName = "TelradRelay"
	linuxServiceUser   = "telrad-relay"
)

var (
	startServiceForAuthentication = enableAndStartService
	executeServiceCommand         = runServiceCommand
)

func printHelp() {
	fmt.Print(`Telrad Relay

Usage:
  telrad                  Authenticate this host and start the relay service
  telrad auth             Authenticate this host and start the relay service
  telrad status           Show the background service status
  telrad start            Start the background service
  telrad stop             Stop the background service
  telrad restart          Restart the background service
  telrad update           Check for a signed Relay update without changing anything
  telrad update VERSION   Install the exact signed version after clinic approval
  telrad doctor           Validate relay configuration and credentials
  telrad ready            Check whether the running relay is ready
  telrad enroll           Authenticate this host again
  telrad rotate-credential Renew the Relay credential now
  telrad retrieval-keygen  Create a local permit key (never replaces an existing key)
  telrad version          Print the installed version

Options:
  --config PATH           Use a different relay configuration file
`)
}

func authenticateAndStart(cfg *config, configPath string) error {
	fmt.Println("Telrad Relay")
	fmt.Println()
	if relayIsEnrolled(cfg) {
		if err := startServiceForAuthentication(); err != nil {
			return err
		}
		fmt.Println("Telrad Relay is already authenticated and running.")
		fmt.Println("Run 'telrad status' for details.")
		return nil
	}
	fmt.Println("Let's connect this host to your Telrad clinic account.")
	if err := enrollForService(cfg, configPath); err != nil {
		return err
	}
	fmt.Println("Authentication complete. Starting the always-on relay service...")
	if err := startServiceForAuthentication(); err != nil {
		return err
	}
	fmt.Println("Telrad is installed, authenticated, and running in the background.")
	return nil
}

func enrollForService(cfg *config, path string) error { return enroll(context.Background(), cfg, path) }
func rotateCredentialForService(cfg *config, path string) error {
	return rotateCredential(context.Background(), cfg)
}

func nativeCommandOutput(cfg *config) io.Writer {
	if cfg.commandOutput != nil {
		return cfg.commandOutput
	}
	return os.Stdout
}

func enableAndStartService() error {
	if runtime.GOOS == "windows" {
		if err := executeServiceCommand("sc.exe", "config", windowsServiceName, "start=", "auto"); err != nil {
			return err
		}
		return executeServiceCommand("sc.exe", "start", windowsServiceName)
	}
	return executeServiceCommand("systemctl", "enable", "--now", linuxServiceName)
}

func serviceAction(action string) error {
	if runtime.GOOS == "windows" {
		windowsAction := action
		if action == "restart" {
			if err := executeServiceCommand("sc.exe", "stop", windowsServiceName); err != nil {
				return err
			}
			windowsAction = "start"
		}
		return executeServiceCommand("sc.exe", windowsAction, windowsServiceName)
	}
	return executeServiceCommand("systemctl", action, linuxServiceName)
}

func disableService() error {
	if runtime.GOOS == "windows" {
		_ = executeServiceCommand("sc.exe", "stop", windowsServiceName)
		return executeServiceCommand("sc.exe", "config", windowsServiceName, "start=", "demand")
	}
	return executeServiceCommand("systemctl", "disable", "--now", linuxServiceName)
}

func serviceStatus() error {
	if runtime.GOOS == "windows" {
		return executeServiceCommand("sc.exe", "query", windowsServiceName)
	}
	return executeServiceCommand("systemctl", "status", "--no-pager", linuxServiceName)
}

func runServiceCommand(name string, args ...string) error {
	if name == "systemctl" {
		name = platformSystemctlPath()
	}
	if name == "sc.exe" {
		directory := platformWindowsSystemDirectory()
		if directory == "" {
			return errors.New("Windows system directory is unavailable")
		}
		name = filepath.Join(directory, name)
	}
	command := exec.Command(name, args...)
	command.Env = serviceCommandEnvironment()
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if runtime.GOOS == "windows" && errors.As(err, &exit) && ((len(args) > 0 && args[0] == "start" && exit.ExitCode() == 1056) || (len(args) > 0 && args[0] == "stop" && exit.ExitCode() == 1062)) {
			return waitPlatformServiceControl(args)
		}
		return fmt.Errorf("service command failed: %w", err)
	}
	return waitPlatformServiceControl(args)
}
