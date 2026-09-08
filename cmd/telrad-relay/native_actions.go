//go:build !relay_container

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
)

func nativeManagementEnabled(path string) bool {
	return managedConfig(path) && (runtime.GOOS == "linux" || runtime.GOOS == "windows")
}

func validateNativeAction(args []string) error {
	if len(args) == 0 {
		return errors.New("native action is required")
	}
	switch args[0] {
	case "auth", "enroll", "rotate-credential", "start", "stop", "restart", "migrate":
		if len(args) != 1 {
			return errors.New("native action accepts no additional arguments")
		}
	case "update":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("native update requires an exact signed version")
		}
		if _, err := parseReleaseVersion(args[1]); err != nil {
			return err
		}
		if len(args) == 3 && !validTransferID(args[2]) {
			return errors.New("invalid update transfer identifier")
		}
	default:
		return errors.New("unknown native action")
	}
	return nil
}

var requestNativeAction = launchNativeAction

func invokeNativeAction(args []string, payload io.Reader) error {
	if err := validateNativeAction(args); err != nil {
		return err
	}
	fmt.Printf("Administrator authorization: Relay %s", args[0])
	if len(args) > 1 {
		fmt.Printf(" %s", args[1])
	}
	fmt.Println(".")
	return requestNativeAction(args, payload)
}

// This entry point is deliberately separate from execute: no general CLI flags,
// configuration parsing, pairing recovery, or service-selected command inputs.
func executeNativeAction(args []string, payload io.Reader) error {
	if distribution != "native" {
		return errors.New("native actions are unavailable in containers")
	}
	if err := validateNativeAction(args); err != nil {
		return err
	}
	if !platformAdministrator() {
		return errors.New("native action requires administrator authorization")
	}
	if err := requireManagedExecutable(); err != nil {
		return err
	}
	if err := validateManagedLayout(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), deviceAuthorizationTimeout+2*time.Minute)
	defer cancel()
	switch args[0] {
	case "start", "stop", "restart":
		return serviceAction(args[0])
	case "auth":
		if err := enableAndStartService(); err != nil {
			return err
		}
		if err := waitForManagement(ctx); err != nil {
			return err
		}
		if err := callManagement(ctx, "doctor", io.Discard); err == nil {
			fmt.Println("Telrad Relay is already authenticated and running.")
			return nil
		}
		return callManagement(ctx, "enroll", os.Stdout)
	case "enroll", "rotate-credential":
		return callManagement(ctx, args[0], os.Stdout)
	case "migrate":
		if err := disableService(); err != nil {
			return err
		}
		return migrateConfig(nativePaths().Config, "", "", "")
	case "update":
		if len(args) == 3 {
			var close func()
			var err error
			payload, close, err = receiveUpdateTransfer(ctx, args[2])
			if err != nil {
				return err
			}
			defer close()
		}
		return installApprovedPayload(ctx, args[1], payload)
	}
	return errors.New("unknown native action")
}

func dispatchNative(command, path string, args []string) (bool, error) {
	if !nativeManagementEnabled(path) {
		return false, nil
	}
	switch command {
	case "auth", "enroll", "rotate-credential", "start", "stop", "restart":
		if len(args) != 0 {
			return true, errors.New("unexpected command arguments")
		}
		return true, invokeNativeAction([]string{command}, nil)
	case "status", "ready", "doctor":
		if len(args) != 0 {
			return true, errors.New("unexpected command arguments")
		}
		err := callManagement(context.Background(), command, os.Stdout)
		if command == "status" {
			return true, errors.Join(err, serviceStatus())
		}
		return true, err
	case "update":
		return true, updateRelay(context.Background(), defaultConfig(), path, args, clientFactory(defaultConfig()).updates)
	}
	return false, nil
}
