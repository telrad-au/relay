//go:build windows && !relay_container

package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestWindowsManagedPathsIgnoreEnvironment(t *testing.T) {
	want := nativePaths()
	t.Setenv("ProgramData", `C:\attacker`)
	t.Setenv("ProgramFiles", `C:\attacker`)
	if nativePaths() != want {
		t.Fatal("environment changed privileged targets")
	}
}
func TestWindowsJunctionCannotRedirectFileOperations(t *testing.T) {
	for _, nested := range []bool{false, true} {
		root := t.TempDir()
		outside := filepath.Join(root, "outside")
		os.Mkdir(outside, 0700)
		directory := filepath.Join(outside, "state")
		os.Mkdir(directory, 0700)
		sentinel := filepath.Join(directory, "relay.json")
		os.WriteFile(sentinel, []byte("keep"), 0600)
		link := filepath.Join(root, "junction")
		destination, target := directory, filepath.Join(link, "relay.json")
		if nested {
			destination, target = outside, filepath.Join(link, "state", "relay.json")
		}
		if data, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, destination).CombinedOutput(); err != nil {
			t.Fatalf("create junction: %v %s", err, data)
		}
		if _, err := safeReadFile(target, 1024); err == nil {
			t.Fatal("junction was followed")
		}
		if err := safeAtomicWrite(target, []byte("changed"), 0600); err == nil {
			t.Fatal("write followed junction")
		}
		if err := prepareNativeDirectory(filepath.Dir(target), true); err == nil {
			t.Fatal("native directory preparation accepted a junction")
		}
		if data, _ := os.ReadFile(sentinel); string(data) != "keep" {
			t.Fatal("junction target changed")
		}
	}
}
func TestWindowsPipeClientCannotLendAdministratorToken(t *testing.T) {
	name := `\\.\pipe\TelradRelay.Test.` + strings.ReplaceAll(filepath.Base(t.TempDir()), "-", "")
	listener, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)(A;;0x00100083;;;AU)"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer c.Close()
		var b [1]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			result <- err
			return
		}
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		fd := c.(interface{ Fd() uintptr }).Fd()
		impersonate := windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")
		ok, _, err := impersonate.Call(fd)
		if ok == 0 {
			result <- err
			return
		}
		defer windows.RevertToSelf()
		var token windows.Token
		if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token); err != nil {
			result <- err
			return
		}
		defer token.Close()
		var level uint32
		var length uint32
		if err := windows.GetTokenInformation(token, windows.TokenImpersonationLevel, (*byte)(unsafe.Pointer(&level)), 4, &length); err != nil {
			result <- err
			return
		}
		if level > uint32(windows.SecurityIdentification) {
			result <- errors.New("client allowed privileged impersonation")
			return
		}
		result <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := winio.DialPipeAccessImpLevel(ctx, name, pipeClientAccess, winio.PipeImpLevelIdentification)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{1})
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	var _ net.Conn = c
}

func TestWindowsPrivateFilesDoNotInheritPublicAccess(t *testing.T) {
	directory := t.TempDir()
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;GRGX;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	d, err := openSafeDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Inspect the creation ACL while the temporary file is still empty: changing
	// it after creation would allow an already-open reader to retain access.
	f, err := createRelativeFile(d.file, "private-temp", 0600, filepath.Join(directory, ".installer-backup-0"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	private, _, err := sd.DACL()
	if err != nil || private == nil {
		t.Fatal("missing private DACL")
	}
	for i := uint32(0); i < uint32(private.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(private, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trusted := sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) || sid.IsWellKnown(windows.WinLocalSystemSid)
		if !token.IsElevated() {
			trusted = trusted || sid.Equals(user.User.Sid)
		}
		if !trusted {
			t.Fatalf("private file granted access to %s", sid.String())
		}
	}
}

func TestWindowsUpdateRollsBackWhileOriginalCLIIsRunning(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "telrad.exe")
	ready := filepath.Join(directory, "ready")
	if err := os.WriteFile(target, original, 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, target, "-test.run=^TestWindowsRunningImageHelper$")
	child.Env = append(os.Environ(), "TELRAD_RUNNING_IMAGE_TEST="+ready)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("image helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	restoreUpdateHandoffSeams(t)
	updateServiceAction = func(string) error { return nil }
	validateApprovedUpdate = func(string, string, string) error { return nil }
	unhealthy := errors.New("synthetic readiness failure")
	waitForApprovedUpdate = func(updateTransaction) error { return unhealthy }
	err = applyUpdateAt(target, filepath.Join(directory, "relay.json"), "2.0.0", []byte("synthetic candidate"))
	if !errors.Is(err, unhealthy) || !strings.Contains(err.Error(), "previous Relay was restored") {
		t.Fatalf("update did not return completed rollback: %v", err)
	}
	restored, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatal("return preceded executable restoration")
	}
}

func TestWindowsRunningImageHelper(t *testing.T) {
	ready := os.Getenv("TELRAD_RUNNING_IMAGE_TEST")
	if ready == "" {
		t.Skip("subprocess helper")
	}
	if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}
