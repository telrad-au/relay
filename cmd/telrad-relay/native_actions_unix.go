//go:build !windows && !relay_container

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

func platformAdministrator() bool { return os.Geteuid() == 0 }
func launchNativeAction(args []string, payload io.Reader) error {
	if err := validateNativeAction(args); err != nil {
		return err
	}
	if platformAdministrator() {
		return executeNativeAction(args, payload)
	}
	command := exec.Command("/usr/bin/sudo", append([]string{"--", nativePaths().Executable, "native-action"}, args...)...)
	command.Stdin = payload
	if payload == nil {
		command.Stdin = os.Stdin
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	return command.Run()
}
func receiveUpdateTransfer(context.Context, string) (io.Reader, func(), error) {
	return nil, func() {}, errors.New("pipe update transfer is Windows-only")
}

func platformSystemctlPath() string { return "/usr/bin/systemctl" }
func serviceCommandEnvironment() []string {
	return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
}

func platformWindowsSystemDirectory() string { return "" }
