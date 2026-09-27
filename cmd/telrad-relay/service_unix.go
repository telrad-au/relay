//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const linuxServiceName = "telrad-relay.service"

func runPlatformService(cfg *config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runRelay(ctx, cfg)
}

func serviceAction(action string) error {
	command := exec.Command("systemctl", action, linuxServiceName)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("systemctl %s failed", action)
		}
		return err
	}
	return nil
}
