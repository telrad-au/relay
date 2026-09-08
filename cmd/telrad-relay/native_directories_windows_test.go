//go:build windows && !relay_container

package main

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func permissiveWindowsInstallParent(t *testing.T) string {
	t.Helper()
	if !platformAdministrator() {
		t.Skip("requires an elevated test process, as used by Windows CI")
	}
	path := t.TempDir()
	sd, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	return path
}

func nativeDirectorySecurity(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd
}

func TestWindowsNativeDirectoryStartsProtected(t *testing.T) {
	parent := permissiveWindowsInstallParent(t)
	for _, name := range []string{"programs", "state"} {
		path := filepath.Join(parent, name)
		serviceWritable := name == "state"
		if err := prepareNativeDirectory(path, serviceWritable); err != nil {
			t.Fatal(err)
		}
		sd := nativeDirectorySecurity(t, path)
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatal("new directory did not block inherited access")
		}
		owner, _, err := sd.Owner()
		if err != nil || owner == nil || !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			t.Fatal("new directory is not administrator-owned")
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 2 {
			t.Fatal("new directory must grant access only to SYSTEM and Administrators")
		}
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, i, &ace); err != nil {
				t.Fatal(err)
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
				t.Fatal("new directory inherited ordinary-user access")
			}
		}
		if err := prepareNativeDirectory(path, serviceWritable); err != nil {
			t.Fatalf("protected directory cannot be reused: %v", err)
		}
		if nativeDirectorySecurity(t, path).String() != sd.String() {
			t.Fatal("preparation changed existing directory permissions")
		}
	}
}

func TestWindowsNativeDirectoryRejectsExistingPublicWrites(t *testing.T) {
	path := filepath.Join(permissiveWindowsInstallParent(t), "existing")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "keep")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	before := nativeDirectorySecurity(t, path).String()
	if err := prepareNativeDirectory(path, false); err == nil {
		t.Fatal("accepted a pre-existing writable installation directory")
	}
	// Existing state has a separate policy: the service must retain its writes.
	if err := prepareNativeDirectory(path, true); err != nil {
		t.Fatalf("existing service-writable state was rejected: %v", err)
	}
	if nativeDirectorySecurity(t, path).String() != before {
		t.Fatal("preparation repaired an existing unsafe ACL")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "unchanged" {
		t.Fatal("preparation changed existing contents")
	}
}
