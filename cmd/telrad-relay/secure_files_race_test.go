//go:build !windows

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileReplacementRaceCannotEscapeDirectory(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	os.Mkdir(state, 0700)
	sentinel := filepath.Join(root, "sentinel")
	original := []byte("outside sentinel")
	os.WriteFile(sentinel, original, 0644)
	target := filepath.Join(state, "relay.json")
	os.WriteFile(target, []byte("inside"), 0600)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			link := filepath.Join(state, "attacker-link")
			_ = os.Symlink(sentinel, link)
			_ = os.Rename(link, target)
			inside := filepath.Join(state, "attacker-file")
			_ = os.WriteFile(inside, []byte("inside"), 0600)
			_ = os.Rename(inside, target)
		}
	}()
	for i := 0; i < 200; i++ {
		data, err := safeReadFile(target, 1024)
		if err == nil && bytes.Equal(data, original) {
			close(stop)
			wg.Wait()
			t.Fatal("read followed raced symlink")
		}
		_ = safeAtomicWrite(target, []byte("inside"), 0600)
	}
	close(stop)
	wg.Wait()
	data, err := os.ReadFile(sentinel)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatal("outside sentinel changed")
	}
	if info, err := os.Stat(sentinel); err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("outside permissions changed")
	}
	if err := safeRemove(filepath.Join(state, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestSafeFilesRejectLinkedAncestor(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside", "state")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "relay.json")
	if err := os.WriteFile(sentinel, []byte("outside sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "managed-parent")
	if err := os.Symlink(filepath.Dir(outside), link); err != nil {
		t.Fatal(err)
	}
	err := safeAtomicWrite(filepath.Join(link, "state", "relay.json"), []byte("replacement"), 0600)
	actual, readErr := os.ReadFile(sentinel)
	if err == nil || readErr != nil || string(actual) != "outside sentinel" {
		t.Fatalf("linked ancestor accepted: write err=%v; outside sentinel=%q, read err=%v", err, actual, readErr)
	}
}

func TestDirectoryCreationRejectsLinkedAncestor(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(filepath.Join(link, "new-directory", "relay.json"), []byte("test"), 0600); err == nil {
		t.Fatal("creation followed ancestor link")
	}
	if _, err := os.Lstat(filepath.Join(outside, "new-directory")); !os.IsNotExist(err) {
		t.Fatal("rejected write created an outside directory")
	}
}
