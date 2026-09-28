//go:build !windows

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLinuxInstall lays out an installation as install.sh would, under a
// temporary root, with a command runner that records instead of running.
func testLinuxInstall(t *testing.T) (*linuxUninstaller, *[]string) {
	t.Helper()
	root := t.TempDir()
	u := &linuxUninstaller{
		unit: filepath.Join(root, "etc/systemd/system", linuxServiceName), libDir: filepath.Join(root, "usr/local/lib/telrad-relay"),
		link: filepath.Join(root, "usr/local/bin/telrad"), configDir: filepath.Join(root, "etc/telrad-relay"),
		dataDir: filepath.Join(root, "var/lib/telrad-relay"), account: "telrad-relay",
	}
	for _, directory := range []string{filepath.Dir(u.unit), u.libDir, filepath.Dir(u.link), u.configDir, u.dataDir} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{
		u.unit: "[Unit]\n", filepath.Join(u.libDir, "telrad"): "binary", filepath.Join(u.configDir, "relay.json"): "{}\n",
		filepath.Join(u.dataDir, identityFileName): "{}\n", filepath.Join(u.dataDir, ledgerFileName): "ACC-SYNTHETIC-1\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(u.libDir, "telrad"), u.link); err != nil {
		t.Fatal(err)
	}
	commands := &[]string{}
	u.run = func(name string, args ...string) error {
		*commands = append(*commands, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	u.accountExists = func(string) bool { return true }
	return u, commands
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestLinuxUninstallKeepsData(t *testing.T) {
	u, commands := testLinuxInstall(t)
	if err := u.remove(false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{u.unit, u.libDir, u.link} {
		if exists(path) {
			t.Fatalf("%s was kept", path)
		}
	}
	for _, path := range []string{filepath.Join(u.configDir, "relay.json"), filepath.Join(u.dataDir, identityFileName), filepath.Join(u.dataDir, ledgerFileName)} {
		if !exists(path) {
			t.Fatalf("%s was removed", path)
		}
	}
	if got := strings.Join(*commands, "; "); got != "systemctl disable --now telrad-relay.service; systemctl daemon-reload" {
		t.Fatalf("commands=%q", got)
	}
	plan := u.plan(false)
	if len(plan.keep) != 3 || !strings.Contains(plan.purgeHint, "rm -rf "+u.configDir+" "+u.dataDir) {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestLinuxUninstallPurge(t *testing.T) {
	u, commands := testLinuxInstall(t)
	if err := u.remove(true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{u.unit, u.libDir, u.link, u.configDir, u.dataDir} {
		if exists(path) {
			t.Fatalf("%s was kept", path)
		}
	}
	if got := strings.Join(*commands, "; "); got != "systemctl disable --now telrad-relay.service; systemctl daemon-reload; userdel telrad-relay" {
		t.Fatalf("commands=%q", got)
	}
	if plan := u.plan(true); len(plan.keep) != 0 || len(plan.remove) != 6 {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestLinuxUninstallLeavesForeignCommandAndToleratesMissingService(t *testing.T) {
	u, commands := testLinuxInstall(t)
	if err := os.Remove(u.link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.link, []byte("someone else's"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(u.unit); err != nil {
		t.Fatal(err)
	}
	u.accountExists = func(string) bool { return false }
	var out bytes.Buffer
	if err := u.remove(true, &out); err != nil {
		t.Fatal(err)
	}
	if !exists(u.link) || !strings.Contains(out.String(), "is not a link to") {
		t.Fatalf("foreign command removed: %q", out.String())
	}
	if got := strings.Join(*commands, "; "); got != "systemctl daemon-reload" {
		t.Fatalf("commands=%q", got)
	}
}

func TestLinuxUninstallStopsOnServiceFailure(t *testing.T) {
	u, _ := testLinuxInstall(t)
	u.run = func(string, ...string) error { return errFake }
	if err := u.remove(true, &bytes.Buffer{}); err != errFake || !exists(u.unit) || !exists(u.dataDir) {
		t.Fatalf("err=%v", err)
	}
}

var errFake = errors.New("fake failure")

func TestManualRemoveCommandQuotes(t *testing.T) {
	if got := manualRemoveCommand("/srv/it's relay"); got != `sudo rm -rf '/srv/it'\''s relay'` {
		t.Fatalf("command=%q", got)
	}
}
