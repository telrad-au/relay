//go:build !windows

package main

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func platformManagedDirectories() (string, string) {
	return "/etc/telrad-relay", "/usr/local/lib/telrad-relay"
}

func openRelativeDirectory(directory *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat directory", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func createRelativeFile(directory *os.File, name string, mode os.FileMode, _ string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "createat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openRelativeRegular(directory *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openCredentialLockHandle(directory *os.File, name, _ string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "openat credential lock", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func lockCredentialHandle(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockCredentialHandle(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func validateRegularHandle(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		return errors.New("Relay file must be regular with exactly one link")
	}
	return nil
}
func syncDirectoryHandle(f *os.File) error { return f.Sync() }
func preserveFileOwner(to, from *os.File) error {
	info, err := from.Stat()
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	return to.Chown(int(stat.Uid), int(stat.Gid))
}

func validateAdministratorHandle(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("managed installation must be root-owned and not writable by group or other users; rerun a reviewed installer")
	}
	return nil
}
