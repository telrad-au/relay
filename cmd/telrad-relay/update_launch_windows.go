//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func launchStagedUpdate(staged string, args []string) error {
	args = append(args, "-parent", fmt.Sprint(os.Getpid()))
	command := exec.Command(staged, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = os.Environ()
	return command.Start()
}
