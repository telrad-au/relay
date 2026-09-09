package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the actual cleanup/export path with a failing Docker command. No
// Docker daemon is needed, and no unrelated container can be addressed.
func TestSetupFailureExportsEvidenceAndCleanupFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake Docker executable")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELAY_PERF_COVERAGE_DIR", "")
	p, err := loadProfile("clinic-v1")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "result")
	r := runner{root: t.TempDir(), out: out, p: p, mode: "smoke", cpus: 1, memory: 256 << 20}
	res, err := r.run(context.Background())
	if err == nil || res.Status != "FAIL" || len(res.CleanupErrors) == 0 {
		t.Fatalf("setup/cleanup failure hidden: %+v %v", res, err)
	}
	if _, err := os.Stat(r.work); !os.IsNotExist(err) {
		t.Fatal("private workspace was not cleaned")
	}
	var saved result
	if err := readJSON(filepath.Join(out, "result.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "FAIL" || len(saved.CleanupErrors) == 0 {
		t.Fatal("failure evidence was not exported")
	}
	if !strings.HasPrefix(r.network, "relay-perf-") {
		t.Fatal("cleanup was not scoped to the run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := runner{root: t.TempDir(), out: filepath.Join(t.TempDir(), "cancelled"), p: p, mode: "smoke", cpus: 1, memory: 256 << 20}
	res, err = cancelled.run(ctx)
	if !errors.Is(err, context.Canceled) || !strings.Contains(strings.Join(res.Reasons, " "), "context canceled") {
		t.Fatalf("cancellation cause was obscured: %v %v", err, res.Reasons)
	}
}

func TestFixtureVolumeCleanupIsScopedAndFailureReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake Docker executable")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "commands")
	t.Setenv("FIXTURE_CLEANUP_COMMANDS", log)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FIXTURE_CLEANUP_COMMANDS\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := runner{root: bin, out: bin, fixtureVolume: "relay-perf-synthetic-test-fixtures"}
	r.cleanup()
	b, err := os.ReadFile(log)
	if err != nil || string(b) != "volume rm relay-perf-synthetic-test-fixtures\n" {
		t.Fatalf("unexpected cleanup commands %q: %v", b, err)
	}
	if len(r.cleanupErrors) != 1 || r.cleanupErrors[0] != "volume_cleanup_failed" {
		t.Fatalf("fixture cleanup failure hidden: %v", r.cleanupErrors)
	}
}
