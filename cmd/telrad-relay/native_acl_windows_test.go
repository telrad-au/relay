//go:build windows && !relay_container

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsNativeACLConfinesChangesToManagedLeaves(t *testing.T) {
	root := permissiveWindowsInstallParent(t)
	directory := filepath.Join(root, "managed")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(directory, "existing-child")
	if err := os.WriteFile(child, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	before := nativeDirectorySecurity(t, child).String()
	// Use a built-in unprivileged SID so this test needs no SCM installation.
	sid, err := windows.CreateWellKnownSid(windows.WinLocalServiceSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := setNativeACLForSID(directory, false, true, sid); err != nil {
		t.Fatalf("secure managed directory: %v", err)
	}
	if nativeDirectorySecurity(t, child).String() != before {
		t.Fatal("directory ACL repair propagated to an unmanaged child")
	}
	if err := setNativeACLForSID(child, false, true, sid); err != nil {
		t.Fatalf("secure managed file: %v", err)
	}
	for _, path := range []string{directory, child} {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = validateAdministratorHandle(file)
		file.Close()
		if err != nil {
			t.Fatalf("managed ACL permits untrusted writes: %v", err)
		}
	}
	before = nativeDirectorySecurity(t, child).String()
	link := filepath.Join(root, "other-name")
	if err := os.Link(child, link); err != nil {
		t.Fatal(err)
	}
	if err := setNativeACLForSID(child, true, false, sid); err == nil {
		t.Fatal("ACL repair accepted a hard-linked target")
	}
	if nativeDirectorySecurity(t, link).String() != before {
		t.Fatal("ACL repair changed a hard-linked target")
	}
}
