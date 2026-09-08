//go:build linux && !relay_container

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func configureNativeTestCA(t *testing.T, path string, _ []byte) {
	t.Helper()
	const directory = "/etc/systemd/system/telrad-relay.service.d"
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	const override = directory + "/native-test-ca.conf"
	if err := safeAtomicWrite(override, []byte(fmt.Sprintf("[Service]\nEnvironment=\"SSL_CERT_FILE=%s\"\n", path)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := reloadNativeService(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = serviceAction("stop")
		_ = safeRemove(override)
		_ = safeRemove(path)
		_ = reloadNativeService()
	})
}

func checkSpoofedNativeEndpoint(t *testing.T) {
	t.Helper()
	dir := filepath.Dir(managementSocket)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir)
	listener, err := net.Listen("unix", managementSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Even a root-owned socket is not the expected unprivileged service peer.
	if err := callManagement(ctx, "doctor", io.Discard); err == nil {
		t.Fatal("accepted a spoofed management endpoint")
	}
}

func TestNativeSystemRollback(t *testing.T) {
	if os.Getenv("TELRAD_NATIVE_LIFECYCLE_TEST") != "1" {
		t.Skip("requires disposable native host")
	}
	links := map[string]string{}
	for _, path := range []string{"/usr/local/bin/telrad", "/etc/systemd/system/multi-user.target.wants/telrad-relay.service"} {
		target, err := os.Readlink(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		links[path] = target
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for path, target := range links {
			_ = os.Remove(path)
			if target != "" {
				if err := os.Symlink(target, path); err != nil {
					t.Error(err)
				}
			}
		}
	})
	restore, err := snapshotNativeSystem()
	if err != nil {
		t.Fatal(err)
	}
	if err := configureNativeService(""); err != nil {
		t.Fatal(err)
	}
	if err := runServiceCommand("/usr/bin/systemctl", "enable", linuxServiceName); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	for path := range links {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("rollback left %s: %v", path, err)
		}
	}
}
