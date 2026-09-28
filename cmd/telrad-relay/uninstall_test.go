package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type fakeUninstaller struct {
	removed []bool // the purge flag of each removal
}

func (u *fakeUninstaller) plan(purge bool) uninstallPlan {
	plan := uninstallPlan{remove: []string{"the service"}, purgeHint: "delete the data by hand"}
	if purge {
		plan.remove = append(plan.remove, "the data")
	} else {
		plan.keep = []string{"the data"}
	}
	return plan
}

func (u *fakeUninstaller) remove(purge bool, _ io.Writer) error {
	u.removed = append(u.removed, purge)
	return nil
}

func TestUninstallConfirmation(t *testing.T) {
	operator := newTestOperator(t)
	platform := &fakeUninstaller{}
	if err := runUninstall(operator.operatorEnv, platform, false, false, ""); err == nil || !strings.Contains(err.Error(), "--yes") || len(platform.removed) != 0 {
		t.Fatalf("no terminal: %v", err)
	}
	operator.answer("n\n")
	if err := runUninstall(operator.operatorEnv, platform, true, false, ""); err != nil || len(platform.removed) != 0 {
		t.Fatalf("declined: %v", err)
	}
	if operator.prompts.String() != "Remove Telrad Relay and permanently delete its pairing and accession ledger? [y/N] " {
		t.Fatalf("purge prompt=%q", operator.prompts.String())
	}
	operator.answer("y\n")
	operator.prompts.Reset()
	operator.output.Reset()
	if err := runUninstall(operator.operatorEnv, platform, false, false, ""); err != nil || len(platform.removed) != 1 || platform.removed[0] {
		t.Fatalf("confirmed: %v %v", err, platform.removed)
	}
	if operator.prompts.String() != "Remove Telrad Relay? [y/N] " {
		t.Fatalf("prompt=%q", operator.prompts.String())
	}
	output := operator.output.String()
	for _, want := range []string{"This removes:\n  the service\n", "This keeps, so a reinstall resumes with the same pairing, report receiver and ledger:\n  the data\n",
		"Telrad Relay was removed.", "Reinstall to resume with the kept files.", "To delete the kept files later: delete the data by hand"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	operator.output.Reset()
	if err := runUninstall(operator.operatorEnv, platform, true, true, ""); err != nil || len(platform.removed) != 2 || !platform.removed[1] {
		t.Fatalf("--purge --yes: %v %v", err, platform.removed)
	}
	if output := operator.output.String(); !strings.Contains(output, "  the data\n") || strings.Contains(output, "This keeps") || !strings.Contains(output, "revoke this Relay") {
		t.Fatalf("purge output:\n%s", output)
	}
}

func TestUninstallRefusals(t *testing.T) {
	operator := newTestOperator(t)
	operator.elevated = func() bool { return false }
	platform := &fakeUninstaller{}
	if err := runUninstall(operator.operatorEnv, platform, false, true, ""); err == nil || !strings.Contains(err.Error(), "changes the installation") || len(platform.removed) != 0 {
		t.Fatalf("unelevated: %v", err)
	}
	if err := uninstallCommand("", []string{"extra"}, &bytes.Buffer{}); err == nil {
		t.Fatal("extra argument accepted")
	}
	useContainer(t)
	err := execute([]string{"uninstall", "--yes"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "remove the Relay container") {
		t.Fatalf("container: %v", err)
	}
}

func TestUninstallNamesCustomDataDir(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "relay data")
	for _, purge := range []bool{false, true} {
		operator := newTestOperator(t)
		platform := &fakeUninstaller{}
		if err := runUninstall(operator.operatorEnv, platform, purge, true, custom); err != nil {
			t.Fatal(err)
		}
		note := custom + ", the dataDir set in relay.json, holds the pairing and ledger and is left in place. Delete it by hand only when retiring the Relay: " + manualRemoveCommand(custom)
		if strings.Count(operator.output.String(), note) != 2 {
			t.Fatalf("purge=%t output:\n%s", purge, operator.output.String())
		}
	}
	if !strings.Contains(manualRemoveCommand(custom), custom) {
		t.Fatalf("command=%q", manualRemoveCommand(custom))
	}
}

func TestSameDirectory(t *testing.T) {
	if !sameDirectory(defaultDataDir(), filepath.Join(defaultDataDir(), ".")) || sameDirectory(defaultDataDir(), t.TempDir()) {
		t.Fatal("sameDirectory")
	}
}
