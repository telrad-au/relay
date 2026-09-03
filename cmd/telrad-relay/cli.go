package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
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
  telrad rotate-credential Rotate the bearer credential
  telrad version          Print the installed version

Options:
  --config PATH           Use a different relay configuration file
`)
}

func shouldElevate(command, configPath string) bool {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 || os.Getenv("TELRAD_RELAY_SERVICE_ENROLL") == "1" {
		return false
	}
	if current, err := user.Current(); err == nil && current.Username == linuxServiceUser {
		return false
	}
	return commandRequiresAdministrator(command, configPath)
}

func commandRequiresAdministrator(command, configPath string) bool {
	switch command {
	case "start", "stop", "restart":
		return true
	case "auth", "enroll", "rotate-credential", "doctor", "migrate-config", "status", "update":
		return configPath == defaultConfigPath()
	default:
		return false
	}
}

func elevateWithSudo(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return errors.New("administrator access is required, but sudo is not installed")
	}
	fmt.Println("Telrad needs administrator access to manage this installation.")
	command := exec.Command("sudo", append([]string{executable}, args...)...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("administrator command failed: %w", err)
	}
	return nil
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
	if err := requireAuthenticationPrivileges(); err != nil {
		return err
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

func requireAuthenticationPrivileges() error {
	switch runtime.GOOS {
	case "linux":
		if os.Geteuid() != 0 {
			return errors.New("first-time authentication requires administrator access")
		}
	case "windows":
		// Service Control Manager returns a clear access-denied error if the
		// terminal is not elevated. Enrollment itself does not need a separate
		// privilege probe on Windows.
	default:
		return fmt.Errorf("automatic service authentication is not supported on %s", runtime.GOOS)
	}
	return nil
}

func enrollForService(cfg *config, configPath string) error {
	if runtime.GOOS != "linux" || configPath != defaultConfigPath() {
		return enroll(context.Background(), cfg, configPath)
	}
	current, err := user.Current()
	if err == nil && current.Username == linuxServiceUser {
		return enroll(context.Background(), cfg, configPath)
	}
	if os.Geteuid() != 0 {
		return errors.New("relay enrollment requires administrator access")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command("runuser", "-u", linuxServiceUser, "--", executable, "--config", configPath, "enroll")
	command.Env = append(os.Environ(), "TELRAD_RELAY_SERVICE_ENROLL=1")
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("relay enrollment failed: %w", err)
	}
	return nil
}

func rotateCredentialForService(cfg *config, configPath string) error {
	if runtime.GOOS != "linux" || configPath != defaultConfigPath() {
		return rotateCredential(context.Background(), cfg)
	}
	current, err := user.Current()
	if err == nil && current.Username == linuxServiceUser {
		return rotateCredential(context.Background(), cfg)
	}
	if os.Geteuid() != 0 {
		return errors.New("credential rotation requires administrator access")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command("runuser", "-u", linuxServiceUser, "--", executable, "--config", configPath, "rotate-credential")
	command.Env = append(os.Environ(), "TELRAD_RELAY_SERVICE_ENROLL=1")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("relay credential rotation failed: %w", err)
	}
	return nil
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
	command := exec.Command(name, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("service command failed: %w", err)
	}
	return nil
}
