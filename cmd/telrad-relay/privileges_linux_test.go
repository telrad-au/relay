//go:build linux

package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxAuthenticationPrivilegeBoundaries(t *testing.T) {
	current, err := user.Current()
	if err == nil && current.Username == linuxServiceUser {
		t.Skip("the service account intentionally bypasses administrator elevation")
	}
	t.Setenv("TELRAD_RELAY_SERVICE_ENROLL", "")
	managedPath := defaultConfigPath()
	customPath := filepath.Join(t.TempDir(), "relay.json")

	if os.Geteuid() == 0 {
		if shouldElevate("status", managedPath) {
			t.Fatal("root process requested sudo elevation")
		}
		if err := requireAuthenticationPrivileges(); err != nil {
			t.Fatal(err)
		}
		return
	}

	if !shouldElevate("status", managedPath) {
		t.Fatal("managed status command did not request elevation")
	}
	if shouldElevate("doctor", customPath) {
		t.Fatal("custom configuration requested elevation")
	}
	if err := requireAuthenticationPrivileges(); err == nil || !strings.Contains(err.Error(), "administrator access") {
		t.Fatalf("authentication privilege error = %v", err)
	}
	if err := enrollForService(defaultConfig(), managedPath); err == nil || !strings.Contains(err.Error(), "administrator access") {
		t.Fatalf("managed enrollment privilege error = %v", err)
	}
	if err := rotateCredentialForService(defaultConfig(), managedPath); err == nil || !strings.Contains(err.Error(), "administrator access") {
		t.Fatalf("managed rotation privilege error = %v", err)
	}

	t.Setenv("TELRAD_RELAY_SERVICE_ENROLL", "1")
	if shouldElevate("status", managedPath) {
		t.Fatal("service-account enrollment recursion requested elevation")
	}
}
