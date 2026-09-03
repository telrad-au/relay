package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCheckRuntimeReadyRequiresFreshHealthyIngest(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	now := time.Now().UTC()
	tests := []struct {
		name      string
		status    relayRuntimeStatus
		wantError bool
	}{
		{name: "ready without control", status: relayRuntimeStatus{State: "ready", UpdatedAt: now.Add(-time.Second), Version: version, ProcessID: 123, IngestReady: true}},
		{name: "authentication attention", status: relayRuntimeStatus{State: "attention", UpdatedAt: now, Version: version, ProcessID: 123, IngestReady: true, AuthenticationAttention: true}, wantError: true},
		{name: "ingest unavailable", status: relayRuntimeStatus{State: "starting", UpdatedAt: now, Version: version, ProcessID: 123}, wantError: true},
		{name: "stale", status: relayRuntimeStatus{State: "ready", UpdatedAt: now.Add(-runtimeStatusMaxAge - time.Second), Version: version, ProcessID: 123, IngestReady: true}, wantError: true},
		{name: "incomplete", status: relayRuntimeStatus{State: "ready", UpdatedAt: now, IngestReady: true}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := atomicWriteJSON(runtimeStatusPath(configPath), test.status); err != nil {
				t.Fatal(err)
			}
			err := checkRuntimeReady(configPath, now)
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%t", err, test.wantError)
			}
		})
	}
}

func TestCheckRuntimeReadyRequiresStatusFile(t *testing.T) {
	if err := checkRuntimeReady(filepath.Join(t.TempDir(), "relay.json"), time.Now()); err == nil {
		t.Fatal("missing runtime status was accepted")
	}
}

func TestRuntimeStatusTransitionsPreserveIndependentAuthenticationSignals(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	manager := newRuntimeStatus(configPath)

	manager.SetIngestReady(true)
	status, err := readRuntimeStatus(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || !status.IngestReady {
		t.Fatalf("ingest-ready status = %+v", status)
	}

	manager.SetControlConnected(true)
	manager.SetControlConnected(true)
	status, err = readRuntimeStatus(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !status.ControlConnected || !status.ReportReturnAvailable || status.State != "ready" {
		t.Fatalf("control-connected status = %+v", status)
	}

	manager.SetAuthenticationAttention(true)
	manager.SetCredentialFileAttention(true)
	manager.SetAuthenticationAttention(false)
	status, err = readRuntimeStatus(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !status.AuthenticationAttention || status.State != "attention" || status.LastErrorCode != "authentication_required" {
		t.Fatalf("credential-file attention was not preserved: %+v", status)
	}

	manager.CredentialAdopted()
	manager.SetCredentialFileAttention(false)
	status, err = readRuntimeStatus(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.AuthenticationAttention || status.State != "ready" || status.LastErrorCode != "" {
		t.Fatalf("credential adoption did not restore readiness: %+v", status)
	}
}
