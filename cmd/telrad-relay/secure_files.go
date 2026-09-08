package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Every operation below uses a held directory and a single leaf name. In
// particular there is no check-then-open of an attacker-selected full path.
type safeDirectory struct {
	root *os.Root
	file *os.File
}

func openSafeDirectory(path string) (*safeDirectory, error) {
	return openDirectory(path, false)
}

func openDirectory(path string, create bool) (*safeDirectory, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(path)
	if strings.ContainsAny(volume, `/\`) {
		return nil, errors.New("Relay directories must be on a local volume")
	}
	anchor := volume + string(os.PathSeparator)
	r, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	f, err := r.Open(".")
	if err != nil {
		r.Close()
		return nil, err
	}
	d := &safeDirectory{r, f}
	for _, name := range strings.Split(strings.TrimPrefix(path, anchor), string(os.PathSeparator)) {
		if name == "" {
			continue
		}
		if !safeLeaf(name) {
			d.Close()
			return nil, errors.New("invalid Relay directory component")
		}
		// Open every component relative to the held parent with OS no-follow
		// semantics. Keep os.Root only after proving it refers to that same inode.
		child, err := openRelativeDirectory(d.file, name)
		if create && errors.Is(err, os.ErrNotExist) {
			if err := d.root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				d.Close()
				return nil, err
			}
			child, err = openRelativeDirectory(d.file, name)
		}
		if err != nil {
			d.Close()
			return nil, err
		}
		root, err := d.root.OpenRoot(name)
		d.Close()
		if err != nil {
			child.Close()
			return nil, err
		}
		opened, err := root.Stat(".")
		info, statErr := child.Stat()
		if err != nil || statErr != nil || !os.SameFile(info, opened) {
			child.Close()
			root.Close()
			return nil, errors.New("Relay directory changed while opening")
		}
		d = &safeDirectory{root, child}
	}
	return d, nil
}

func (d *safeDirectory) Close() { d.file.Close(); d.root.Close() }

func safeLeaf(name string) bool {
	return name != "." && name != ".." && filepath.IsLocal(name) && filepath.Base(name) == name && !strings.ContainsAny(name, `:/\`) && !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, " ")
}

func (d *safeDirectory) open(name string) (*os.File, error) {
	if !safeLeaf(name) {
		return nil, errors.New("invalid Relay file name")
	}
	f, err := openRelativeRegular(d.file, name)
	if err != nil {
		return nil, err
	}
	if err := validateRegularHandle(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func safeReadFile(path string, limit int64) ([]byte, error) {
	d, err := openSafeDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readBoundedFile(f, limit)
}

func readBoundedFile(f *os.File, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		return nil, errors.New("Relay file exceeds size limit")
	}
	return data, err
}

func (d *safeDirectory) check(name string) error {
	f, err := d.open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		f.Close()
	}
	return err
}

func safeRemove(path string) error {
	d, err := openSafeDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	name := filepath.Base(path)
	if err := d.check(name); err != nil {
		return err
	}
	return d.root.Remove(name)
}

func safeRename(from, to string) error {
	if !samePath(filepath.Dir(from), filepath.Dir(to)) {
		return errors.New("Relay transaction must stay in one directory")
	}
	d, err := openSafeDirectory(filepath.Dir(from))
	if err != nil {
		return err
	}
	defer d.Close()
	for _, path := range []string{from, to} {
		if err := d.check(filepath.Base(path)); err != nil {
			return err
		}
	}
	return d.root.Rename(filepath.Base(from), filepath.Base(to))
}

func safeAtomicWrite(path string, data []byte, mode os.FileMode) (returnErr error) {
	d, err := openSafeDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	name := filepath.Base(path)
	if !safeLeaf(name) {
		return errors.New("invalid Relay file name")
	}
	previous, err := d.open(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if previous != nil {
		defer previous.Close()
	}
	tempName := fmt.Sprintf(".telrad-write-%x", rand.Text())
	temp, err := createRelativeFile(d.file, tempName, mode, path)
	if err != nil {
		return err
	}
	defer func() { temp.Close(); _ = d.root.Remove(tempName) }()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if previous != nil {
		if err := preserveFileOwner(temp, previous); err != nil {
			return err
		}
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := d.check(name); err != nil {
		return err
	}
	if err := d.root.Rename(tempName, name); err != nil {
		return err
	}
	return syncDirectoryHandle(d.file)
}
