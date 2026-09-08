//go:build relay_container

package main

import (
	"context"
	"errors"
	"io"
)

func nativeManagementEnabled(string) bool                   { return false }
func dispatchNative(string, string, []string) (bool, error) { return false, nil }
func executeNativeAction([]string, io.Reader) error {
	return errors.New("native actions are unavailable in containers")
}
func invokeNativeAction([]string, io.Reader) error {
	return errors.New("native actions are unavailable in containers")
}
func runNativeManagement(context.Context, *config, string) error {
	return errors.New("native management is unavailable in containers")
}
func platformAdministrator() bool { return false }
func submitApprovedUpdate(updateRelease, []byte) error {
	return errors.New("native updates are unavailable in containers")
}
func updatePreparationReady(string) error {
	return errors.New("native updates are unavailable in containers")
}
func requireServiceIdentity() error { return nil }

func callUpdatedDiagnostics() error {
	return errors.New("native updates are unavailable in containers")
}

func installNative(io.Reader) error {
	return errors.New("native installation is unavailable in containers")
}

func platformSystemctlPath() string       { return "" }
func serviceCommandEnvironment() []string { return nil }

func platformWindowsSystemDirectory() string { return "" }
