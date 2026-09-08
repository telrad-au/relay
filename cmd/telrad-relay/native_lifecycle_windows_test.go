//go:build windows && !relay_container

package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"golang.org/x/sys/windows/svc/mgr"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func configureNativeTestCA(t *testing.T, path string, certificate []byte) {
	t.Helper()
	certutil := filepath.Join(platformWindowsSystemDirectory(), "certutil.exe")
	if out, err := exec.Command(certutil, "-addstore", "Root", path).CombinedOutput(); err != nil {
		t.Fatalf("add disposable test CA: %v: %s", err, out)
	}
	thumbprint := sha1.Sum(certificate)
	t.Cleanup(func() {
		_ = serviceAction("stop")
		_ = exec.Command(certutil, "-delstore", "Root", hex.EncodeToString(thumbprint[:])).Run()
		_ = safeRemove(path)
	})
}

func checkSpoofedNativeEndpoint(t *testing.T) {
	t.Helper()
	listener, err := winio.ListenPipe(managementPipe+".Read", &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;BA)(A;;GA;;;SY)"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			var b [1]byte
			conn.Read(b[:])
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := callManagement(ctx, "doctor", io.Discard); err == nil {
		t.Fatal("accepted a spoofed management endpoint")
	}
	listener.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("spoofed pipe connection leaked")
	}
}

func TestNativeSystemRollback(t *testing.T) {
	if os.Getenv("TELRAD_NATIVE_LIFECYCLE_TEST") != "1" {
		t.Skip("requires disposable native host")
	}
	if err := serviceAction("stop"); err != nil {
		t.Fatal(err)
	}
	scm, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer scm.Disconnect()
	service, err := scm.OpenService(windowsServiceName)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	before, err := service.Config()
	if err != nil {
		t.Fatal(err)
	}
	restore, err := snapshotNativeSystem()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	changed := before
	changed.StartType = mgr.StartManual
	changed.SidType = 0
	if err := service.UpdateConfig(changed); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	after, err := service.Config()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("SCM settings not restored: %v", err)
	}
}
