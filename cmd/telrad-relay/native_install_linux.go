//go:build linux && !relay_container

package main

import (
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
)

//go:embed telrad-relay.service
var nativeSystemdUnit string

func prepareNativeInstallation() error {
	if _, err := user.Lookup(linuxServiceUser); err != nil {
		if err := runServiceCommand("/usr/sbin/useradd", "--system", "--home-dir", "/var/lib/telrad-relay", "--shell", "/usr/sbin/nologin", linuxServiceUser); err != nil {
			return err
		}
	}
	for _, path := range []string{"/usr/local/lib/telrad-relay", "/etc/telrad-relay"} {
		parent, err := openSafeDirectory(filepath.Dir(path))
		if err != nil {
			return err
		}
		if err := validateAdministratorHandle(parent.file); err != nil {
			parent.Close()
			return err
		}
		err = parent.root.Mkdir(filepath.Base(path), 0700)
		parent.Close()
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		d, err := openSafeDirectory(path)
		if err != nil {
			return err
		}
		if path == "/usr/local/lib/telrad-relay" {
			err = validateAdministratorHandle(d.file)
			if err == nil {
				err = d.file.Chmod(0755)
			}
		}
		d.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func nativeServiceRunning() (bool, error) {
	c := exec.Command("/usr/bin/systemctl", "is-active", "--quiet", linuxServiceName)
	c.Env = serviceCommandEnvironment()
	err := c.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, err
}
func nativeServiceFiles() []string { return []string{"/etc/systemd/system/telrad-relay.service"} }
func reloadNativeService() error   { return runServiceCommand("/usr/bin/systemctl", "daemon-reload") }

func snapshotNativeSystem() (func() error, error) {
	// These are the only links created by the CLI installer and systemctl enable.
	links := map[string]string{
		"/usr/local/bin/telrad": "",
		"/etc/systemd/system/multi-user.target.wants/telrad-relay.service": "",
	}
	for path := range links {
		d, err := openSafeDirectory(filepath.Dir(path))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := validateAdministratorHandle(d.file); err != nil {
			d.Close()
			return nil, err
		}
		target, err := d.root.Readlink(filepath.Base(path))
		d.Close()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if target != "" {
			resolved := absolute(filepath.Dir(path), target)
			if resolved != nativePaths().Executable && resolved != "/var/lib/telrad-relay/bin/telrad" && resolved != nativeServiceFiles()[0] {
				return nil, errors.New("unexpected installer-managed link target")
			}
		}
		links[path] = target
	}
	return func() error {
		var result error
		for path, target := range links {
			d, err := openSafeDirectory(filepath.Dir(path))
			if errors.Is(err, os.ErrNotExist) && target == "" {
				continue
			}
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			if err = validateAdministratorHandle(d.file); err == nil {
				err = d.root.Remove(filepath.Base(path))
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
				if err == nil && target != "" {
					err = d.root.Symlink(target, filepath.Base(path))
				}
			}
			d.Close()
			result = errors.Join(result, err)
		}
		return result
	}, nil
}
func configureNativeService(_ string) error {
	if err := safeAtomicWrite(nativeServiceFiles()[0], []byte(nativeSystemdUnit), 0644); err != nil {
		return err
	}
	d, err := openSafeDirectory("/usr/local/bin")
	if err != nil {
		return err
	}
	defer d.Close()
	if err := validateAdministratorHandle(d.file); err != nil {
		return err
	}
	const link = "telrad"
	if info, err := d.root.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("/usr/local/bin/telrad is not an installer-managed link")
		}
		target, err := d.root.Readlink(link)
		if err != nil {
			return err
		}
		if target != nativePaths().Executable && target != "/var/lib/telrad-relay/bin/telrad" {
			return errors.New("unexpected CLI link target")
		}
		if err := d.root.Remove(link); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := d.root.Symlink(nativePaths().Executable, link); err != nil {
		return err
	}
	return runServiceCommand("/usr/bin/systemctl", "daemon-reload")
}
func secureNativeState() error {
	paths := nativePaths()
	for _, path := range append([]string{paths.Executable, paths.Trust, paths.Installation}, nativeServiceFiles()...) {
		d, err := openSafeDirectory(filepath.Dir(path))
		if err != nil {
			return err
		}
		f, err := d.open(filepath.Base(path))
		d.Close()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if samePath(path, paths.Executable) {
			mode = 0755
		}
		err = f.Chown(0, 0)
		if err == nil {
			err = f.Chmod(mode)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	u, err := user.Lookup(linuxServiceUser)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	d, err := openSafeDirectory(filepath.Dir(nativePaths().Config))
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.file.Chmod(0700); err != nil {
		return err
	}
	if err := d.file.Chown(uid, gid); err != nil {
		return err
	}
	for _, name := range []string{"relay.json", "relay-credential.json", permitKeyFilename, "runtime-status.json", "relay.json.pairing-transaction.json", "relay.json.next", "relay.json.previous", "relay-credential.json.next", "relay-credential.json.previous"} {
		f, err := d.open(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		err = f.Chmod(0600)
		if err == nil {
			err = f.Chown(uid, gid)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
