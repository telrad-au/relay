//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// linuxUninstaller removes what packaging/install.sh installs. The paths are
// fields so tests can point them at a temporary directory.
type linuxUninstaller struct {
	unit, libDir, link, configDir, dataDir, account string
	run                                             func(name string, args ...string) error
	accountExists                                   func(name string) bool
}

func nativeUninstaller() uninstaller {
	return &linuxUninstaller{
		unit: "/etc/systemd/system/" + linuxServiceName, libDir: "/usr/local/lib/telrad-relay", link: "/usr/local/bin/telrad",
		configDir: "/etc/telrad-relay", dataDir: defaultDataDir(), account: "telrad-relay",
		run: runCommand,
		accountExists: func(name string) bool {
			_, err := user.Lookup(name)
			return err == nil
		},
	}
}

func (u *linuxUninstaller) plan(purge bool) uninstallPlan {
	plan := uninstallPlan{remove: []string{
		linuxServiceName + " (" + u.unit + ")",
		u.libDir + " (the program)",
		u.link + " (the telrad command)",
	}}
	data := []string{
		u.configDir + " (relay.json: the report receiver)",
		u.dataDir + " (identity.json: the pairing; accessions.ledger: the accession ledger)",
	}
	if purge {
		plan.remove = append(plan.remove, data...)
		plan.remove = append(plan.remove, "the "+u.account+" system user")
		return plan
	}
	plan.keep = append(data, "the "+u.account+" system user")
	plan.purgeHint = fmt.Sprintf("sudo rm -rf %s %s && sudo userdel %s", u.configDir, u.dataDir, u.account)
	return plan
}

func (u *linuxUninstaller) remove(purge bool, out io.Writer) error {
	if _, err := os.Stat(u.unit); err == nil {
		if err := u.run("systemctl", "disable", "--now", linuxServiceName); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := removePath(u.unit); err != nil {
		return err
	}
	// Remove the command only while it is the link the installer made.
	target := filepath.Join(u.libDir, "telrad")
	if destination, err := os.Readlink(u.link); err == nil && destination == target {
		if err := removePath(u.link); err != nil {
			return err
		}
	} else if _, statErr := os.Lstat(u.link); statErr == nil {
		fmt.Fprintf(out, "Left %s in place because it is not a link to %s.\n", u.link, target)
	}
	if err := removePath(u.libDir); err != nil {
		return err
	}
	if err := u.run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if !purge {
		return nil
	}
	for _, path := range []string{u.configDir, u.dataDir} {
		if err := removePath(path); err != nil {
			return err
		}
	}
	if u.accountExists(u.account) {
		return u.run("userdel", u.account)
	}
	return nil
}

func removePath(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func sameDirectory(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

// manualRemoveCommand is the shell command that deletes directory.
func manualRemoveCommand(directory string) string {
	return "sudo rm -rf '" + strings.ReplaceAll(directory, "'", `'\''`) + "'"
}
