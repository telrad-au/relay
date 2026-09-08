//go:build windows && !relay_container

package main

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestWindowsIntegrationErrorsUsePlainText(t *testing.T) {
	// An invalid operation exercises the real PowerShell error stream without
	// changing firewall rules, the machine PATH, or any service configuration.
	integration, err := windowsIntegrationCommand("invalid-test-operation", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, integration.Path, integration.Args[1:]...)
	command.Env = integration.Env
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if err == nil {
		t.Fatal("invalid installation operation succeeded")
	}
	if !bytes.Contains(output, []byte("Invalid installation operation.")) {
		t.Fatalf("missing installation error: %s", output)
	}
	if bytes.Contains(output, []byte("CLIXML")) || bytes.Contains(output, []byte("<Objs")) {
		t.Fatalf("installation error was XML instead of plain text: %s", output)
	}
}
