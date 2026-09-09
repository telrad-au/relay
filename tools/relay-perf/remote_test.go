package main

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRemoteExtractionPreservesPublicCAAndPrivateConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote extraction runs on Linux")
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for name, mode := range map[string]int64{"ca.pem": 0644, "worker.json": 0600} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	command := exec.Command("sh", "-c", remoteExtract+".")
	command.Dir, command.Stdin = dir, &archive
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("extract: %v: %s", err, output)
	}
	for name, mode := range map[string]os.FileMode{"ca.pem": 0644, "worker.json": 0600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s permissions: %v, %v", name, info, err)
		}
	}
}

func TestRemoteWorkspaceCleanupScope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the remote Docker controller uses Linux filesystem paths")
	}
	for _, path := range []string{"/tmp/relay-perf-123", "/var/tmp/relay-perf-123"} {
		if !validRemoteWorkspace(path, "relay-perf-123") {
			t.Fatal("rejected run-owned workspace")
		}
	}
	for _, path := range []string{"/tmp", "/var/tmp", "/var/tmp/../private", "/home/relay-perf-123", "/tmp/relay-perf-123;true"} {
		if validRemoteWorkspace(path, "relay-perf-123") {
			t.Fatal("accepted unowned cleanup path")
		}
	}
}

func TestRemoteTargetIsPrivateAndSeparate(t *testing.T) {
	for _, tt := range []struct {
		endpoint, relay, support string
		valid                    bool
	}{
		{"ssh://ec2-user@10.80.0.10", "10.80.0.10", "10.80.0.11", true},
		{"tcp://10.80.0.10:2375", "10.80.0.10", "10.80.0.11", false},
		{"ssh://ec2-user@10.80.0.10", "10.80.0.10", "10.80.0.10", false},
		{"ssh://ec2-user@8.8.8.8", "8.8.8.8", "10.80.0.11", false},
		{"ssh://ec2-user:secret@10.80.0.10", "10.80.0.10", "10.80.0.11", false},
		{"ssh://ec2-user@10.80.0.10/bad", "10.80.0.10", "10.80.0.11", false},
	} {
		if got := validateRemoteDocker(tt.endpoint, tt.relay, tt.support) == nil; got != tt.valid {
			t.Fatalf("remote validation: %+v", tt)
		}
	}
}

func TestRemoteDockerRoutesOnlyOwnedTargetResources(t *testing.T) {
	r := runner{remoteDocker: "ssh://ec2-user@10.80.0.10", volume: "relay-perf-1-state", remoteContainers: map[string]bool{"relay-perf-1-relay-1": true}}
	for _, tt := range []struct {
		args   []string
		remote bool
	}{
		{[]string{"stop", "relay-perf-1-relay-1"}, true},
		{[]string{"logs", "relay-perf-1-relay-1"}, true},
		{[]string{"volume", "create", "relay-perf-1-state"}, true},
		{[]string{"volume", "rm", "relay-perf-1-fixtures"}, false},
		{[]string{"stop", "some-other-container"}, false},
		{[]string{"network", "rm", "relay-perf-1"}, false},
		{[]string{"build", "."}, false},
	} {
		if got := r.remoteCommand(tt.args); got != tt.remote {
			t.Fatalf("misrouted %v", tt.args)
		}
	}
}
