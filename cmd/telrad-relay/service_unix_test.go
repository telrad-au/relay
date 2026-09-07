//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUnixServiceCommandsUseExpectedSystemdContract(t *testing.T) {
	original := executeServiceCommand
	t.Cleanup(func() { executeServiceCommand = original })
	var commands []string
	executeServiceCommand = func(name string, args ...string) error {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil
	}
	for _, run := range []func() error{enableAndStartService, func() error { return serviceAction("restart") }, disableService, serviceStatus} {
		if err := run(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"systemctl enable --now " + linuxServiceName, "systemctl restart " + linuxServiceName, "systemctl disable --now " + linuxServiceName, "systemctl status --no-pager " + linuxServiceName}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands=%q, want %q", commands, want)
	}
	if err := runServiceCommand("/bin/sh", "-c", "exit 7"); err == nil {
		t.Fatal("service command failure was ignored")
	}
}

func TestValidateInstalledUpdateRunsVersionAndDoctor(t *testing.T) {
	directory := t.TempDir()
	target := writeUnixTestCommand(t, directory, "telrad", `#!/bin/sh
case "$1" in
    version)
        printf '%s\n' '2.0.0'
        ;;
    --config)
        [ "$2" = "$TELRAD_RELAY_TEST_CONFIG" ] && [ "$3" = 'doctor' ] || exit 4
        if [ "${TELRAD_RELAY_TEST_DOCTOR_FAIL:-}" = '1' ]; then
            printf '%s\n' 'doctor failed' >&2
            exit 5
        fi
        ;;
    *)
        exit 6
        ;;
esac
`)
	configPath := filepath.Join(directory, "relay.json")
	t.Setenv("TELRAD_RELAY_TEST_CONFIG", configPath)
	if err := validateInstalledUpdate(target, configPath, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := validateInstalledUpdate(target, configPath, "2.0.1"); err == nil || !strings.Contains(err.Error(), "expected \"2.0.1\"") {
		t.Fatalf("version mismatch error = %v", err)
	}

}

func TestNonWindowsPlatformServiceDelegatesToRelayRunner(t *testing.T) {
	original := runNonWindowsService
	t.Cleanup(func() { runNonWindowsService = original })
	wantErr := errors.New("runner stopped")
	cfg := defaultConfig()
	configPath := filepath.Join(t.TempDir(), "relay.json")
	runNonWindowsService = func(gotConfig *config, gotPath string) error {
		if gotConfig != cfg || gotPath != configPath {
			t.Fatalf("runner inputs = %p, %q", gotConfig, gotPath)
		}
		return wantErr
	}
	if err := runPlatformService(cfg, configPath); !errors.Is(err, wantErr) {
		t.Fatalf("platform service error = %v", err)
	}
}

func writeUnixTestCommand(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
