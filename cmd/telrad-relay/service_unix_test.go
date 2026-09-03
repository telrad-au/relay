//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUnixServiceCommandsUseExpectedSystemdContract(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "systemctl.log")
	writeUnixTestCommand(t, directory, "systemctl", `#!/bin/sh
printf '%s\n' "$*" >> "$TELRAD_RELAY_TEST_COMMAND_LOG"
exit "${TELRAD_RELAY_TEST_EXIT_CODE:-0}"
`)
	t.Setenv("PATH", directory)
	t.Setenv("TELRAD_RELAY_TEST_COMMAND_LOG", logPath)

	for _, run := range []func() error{
		enableAndStartService,
		func() error { return serviceAction("restart") },
		disableService,
		serviceStatus,
	} {
		if err := run(); err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(content)), "\n")
	want := []string{
		"enable --now " + linuxServiceName,
		"restart " + linuxServiceName,
		"disable --now " + linuxServiceName,
		"status --no-pager " + linuxServiceName,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("service commands = %q, want %q", got, want)
	}

	t.Setenv("TELRAD_RELAY_TEST_EXIT_CODE", "7")
	if err := serviceAction("stop"); err == nil || !strings.Contains(err.Error(), "service command failed") {
		t.Fatalf("failed service command error = %v", err)
	}
}

func TestElevateWithSudoForwardsExecutableAndArguments(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "sudo.log")
	writeUnixTestCommand(t, directory, "sudo", `#!/bin/sh
printf '%s\n' "$@" > "$TELRAD_RELAY_TEST_COMMAND_LOG"
`)
	t.Setenv("PATH", directory)
	t.Setenv("TELRAD_RELAY_TEST_COMMAND_LOG", logPath)

	wantArgs := []string{"--config", "/tmp/relay.json", "status"}
	if err := elevateWithSudo(wantArgs); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(got) != len(wantArgs)+1 || !reflect.DeepEqual(got[1:], wantArgs) {
		t.Fatalf("sudo arguments = %q", got)
	}
	if executable, err := os.Executable(); err != nil || got[0] != executable {
		t.Fatalf("sudo executable = %q, current executable = %q, error = %v", got[0], executable, err)
	}

	t.Setenv("PATH", t.TempDir())
	if err := elevateWithSudo(nil); err == nil || !strings.Contains(err.Error(), "sudo is not installed") {
		t.Fatalf("missing sudo error = %v", err)
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
	t.Setenv("TELRAD_RELAY_TEST_DOCTOR_FAIL", "1")
	if err := validateInstalledUpdate(target, configPath, "2.0.0"); err == nil || !strings.Contains(err.Error(), "doctor failed") {
		t.Fatalf("doctor failure error = %v", err)
	}
}

func TestUnixProcessAndLaunchBoundaries(t *testing.T) {
	process := exec.Command("/bin/sh", "-c", "exit 0")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	processID := process.Process.Pid
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := waitForProcessExit(processID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForProcessExit(os.Getpid(), 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("live process wait error = %v", err)
	}
	if err := launchStagedUpdate(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("missing staged executable was launched")
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
