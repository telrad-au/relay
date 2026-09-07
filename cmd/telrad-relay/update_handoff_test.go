package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestApprovedUpdateCompletesTransaction(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "telrad")
	configPath := filepath.Join(directory, "relay.json")
	oldBinary := []byte("old relay")
	newBinary := []byte("new relay")
	if err := os.WriteFile(target, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}

	restoreUpdateHandoffSeams(t)
	var serviceActions []string
	updateServiceAction = func(action string) error {
		serviceActions = append(serviceActions, action)
		return nil
	}
	validateApprovedUpdate = func(gotTarget, gotConfigPath, expectedVersion string) error {
		if gotTarget != target || gotConfigPath != configPath || expectedVersion != "2.0.0" {
			t.Fatalf("validation inputs = %q, %q, %q", gotTarget, gotConfigPath, expectedVersion)
		}
		installed, err := os.ReadFile(gotTarget)
		if err != nil {
			return err
		}
		if !bytes.Equal(installed, newBinary) {
			t.Fatalf("validated binary = %q", installed)
		}
		return nil
	}
	var transaction updateTransaction
	waitForApprovedUpdate = func(value updateTransaction) error {
		transaction = value
		return nil
	}

	if err := applyUpdateAt(target, configPath, "2.0.0", newBinary); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serviceActions, []string{"stop", "start"}) {
		t.Fatalf("service actions = %v", serviceActions)
	}
	installed, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installed, newBinary) {
		t.Fatalf("installed binary = %q", installed)
	}
	if transaction.Target != target || transaction.Previous != target+".previous" || transaction.ConfigPath != configPath || transaction.ExpectedVersion != "2.0.0" {
		t.Fatalf("transaction = %+v", transaction)
	}
	if transaction.ActivatedAt.IsZero() || transaction.Deadline.Sub(transaction.ActivatedAt) != updateHealthTimeout {
		t.Fatalf("transaction timing = %+v", transaction)
	}
	for _, removed := range []string{target + ".previous", updateJournalPath(target)} {
		if _, err := os.Stat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed update retained %s: %v", removed, err)
		}
	}
}

func TestApprovedUpdateRestoresPreviousBinaryAfterHealthFailure(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "telrad")
	configPath := filepath.Join(directory, "relay.json")
	oldBinary := []byte("known-good relay")
	newBinary := []byte("unhealthy relay")
	if err := os.WriteFile(target, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}

	oldConfig := []byte(`{"schemaVersion":3}`)
	if err := os.WriteFile(configPath, oldConfig, 0600); err != nil {
		t.Fatal(err)
	}
	restoreUpdateHandoffSeams(t)
	var serviceActions []string
	updateServiceAction = func(action string) error {
		serviceActions = append(serviceActions, action)
		return nil
	}
	validateApprovedUpdate = func(gotTarget, _, _ string) error {
		installed, err := os.ReadFile(gotTarget)
		if err != nil {
			return err
		}
		if !bytes.Equal(installed, newBinary) {
			t.Fatalf("binary before readiness check = %q", installed)
		}
		return nil
	}
	healthFailure := errors.New("health check failed")
	waitForApprovedUpdate = func(updateTransaction) error {
		if err := os.WriteFile(configPath, []byte(`{"schemaVersion":4}`), 0600); err != nil {
			t.Fatal(err)
		}
		return healthFailure
	}

	err := applyUpdateAt(target, configPath, "2.0.0", newBinary)
	if err == nil || !errors.Is(err, healthFailure) {
		t.Fatalf("update error = %v", err)
	}
	if !reflect.DeepEqual(serviceActions, []string{"stop", "start", "stop", "start"}) {
		t.Fatalf("service actions = %v", serviceActions)
	}
	restored, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(restored, oldBinary) {
		t.Fatalf("restored binary = %q", restored)
	}
	restoredConfig, configErr := os.ReadFile(configPath)
	if configErr != nil || !bytes.Equal(restoredConfig, oldConfig) {
		t.Fatal("rollback did not restore pre-upgrade configuration")
	}
	for _, removed := range []string{target + ".previous", target + ".config.previous", updateJournalPath(target)} {
		if _, statErr := os.Stat(removed); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rollback retained %s: %v", removed, statErr)
		}
	}
}

func TestApplyStagedUpdateRequiresCompleteSafeArguments(t *testing.T) {
	tests := [][]string{
		nil,
		{"-target", "telrad", "-version", "2.0.0"},
		{"-target", "telrad", "-version", "2.0.0", "-config", "relay.json", "-parent", "-1"},
	}
	for _, args := range tests {
		if err := execute(append([]string{"apply-update"}, args...)); err == nil {
			t.Fatalf("unsafe arguments %q were accepted", args)
		}
	}
}

func restoreUpdateHandoffSeams(t *testing.T) {
	t.Helper()
	originalServiceAction := updateServiceAction
	originalValidate := validateApprovedUpdate
	originalWaitForReady := waitForApprovedUpdate
	t.Cleanup(func() {
		updateServiceAction = originalServiceAction
		validateApprovedUpdate = originalValidate
		waitForApprovedUpdate = originalWaitForReady
	})
}
