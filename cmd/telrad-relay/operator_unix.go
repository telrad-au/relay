//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	elevationHint       = "run it as root, for example with sudo"
	serviceDisplayName  = linuxServiceName
	reportReceiverUsage = "sudo telrad report-receiver HOST[:PORT]"
)

func isElevated() bool { return os.Geteuid() == 0 }

// openTerminal opens the controlling terminal, which is there even when
// standard input or output is redirected and absent under CI or a service.
func openTerminal() *terminal {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	return &terminal{in: tty, out: tty, close: tty.Close}
}

// runCommand runs a system command, returning its output in the error when it
// fails. The commands run here print unit names and paths, nothing secret.
func runCommand(name string, args ...string) error {
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		var exit *exec.ExitError
		if errors.As(err, &exit) && detail != "" {
			return fmt.Errorf("%s %s failed: %s", name, strings.Join(args, " "), detail)
		}
		return fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

type systemdService struct {
	run func(name string, args ...string) error
}

func nativeService() serviceControl { return systemdService{run: runCommand} }

func (service systemdService) running() (bool, error) {
	err := exec.Command("systemctl", "is-active", "--quiet", linuxServiceName).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return err == nil, err
}

func (service systemdService) start() error {
	return service.run("systemctl", "start", linuxServiceName)
}

func (service systemdService) stop() error {
	return service.run("systemctl", "stop", linuxServiceName)
}

func (service systemdService) restart() error {
	return service.run("systemctl", "restart", linuxServiceName)
}
