package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPairingLink = "https://app.example.invalid/relays/approve/enrol-1"

// pairingDataDir gives cfg a ledger and an open backlog window, which pairing
// again must never touch.
func pairingDataDir(t *testing.T, cfg *config) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		ledgerFileName:        []byte("ACC-SYNTHETIC-1\n"),
		acceptBacklogFileName: []byte(`{"until":"2099-01-01T00:00:00Z"}` + "\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(cfg.dataPath(name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func assertFilesKept(t *testing.T, cfg *config, files map[string][]byte) {
	t.Helper()
	for name, want := range files {
		if got, err := os.ReadFile(cfg.dataPath(name)); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed: %q %v", name, got, err)
		}
	}
}

func identityExists(cfg *config) bool {
	_, err := os.Stat(cfg.dataPath(identityFileName))
	return err == nil
}

// pairedOperator is a running, paired native Relay whose restarted service
// publishes a pairing link.
func pairedOperator(t *testing.T) (*testOperator, *config, map[string][]byte) {
	t.Helper()
	pki := newTestPKI(t)
	cfg := testConfig(t, pki)
	pairedStore(t, cfg, pki, telradEndpoints{Host: "127.0.0.1", DicomPort: 1, HL7Port: 1, ReportPort: 1})
	kept := pairingDataDir(t, cfg)
	operator := newTestOperator(t)
	operator.service.isRunning = true
	operator.status.set(&statusReport{State: "ready", Paired: true, RelayID: "relay-test"})
	operator.service.onStart = func() {
		operator.status.set(&statusReport{State: "pairing", PairingLink: testPairingLink})
	}
	return operator, cfg, kept
}

func TestPairNativeRefusesWithoutTerminalOrYes(t *testing.T) {
	operator, cfg, kept := pairedOperator(t)
	err := pairNative(operator.operatorEnv, cfg, false)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(operator.output.String(), "This Relay is paired: Relay relay-test, certificate expires ") {
		t.Fatalf("output=%q", operator.output.String())
	}
	if !identityExists(cfg) || operator.service.history() != "" {
		t.Fatalf("identity or service changed: %q", operator.service.history())
	}
	assertFilesKept(t, cfg, kept)
}

func TestPairNativeDeclined(t *testing.T) {
	operator, cfg, kept := pairedOperator(t)
	operator.answer("n\n")
	if err := pairNative(operator.operatorEnv, cfg, false); err != nil {
		t.Fatal(err)
	}
	if operator.prompts.String() != "Pair this Relay again? [y/N] " || !strings.Contains(operator.output.String(), "Nothing was changed.") {
		t.Fatalf("prompt=%q output=%q", operator.prompts.String(), operator.output.String())
	}
	if !identityExists(cfg) || operator.service.history() != "" {
		t.Fatal("declined pairing changed the installation")
	}
	assertFilesKept(t, cfg, kept)
}

func TestPairNativeReplacesOnlyIdentity(t *testing.T) {
	for _, test := range []struct {
		name  string
		yes   bool
		reply string
	}{{"confirmed", false, "y\n"}, {"--yes", true, ""}} {
		t.Run(test.name, func(t *testing.T) {
			operator, cfg, kept := pairedOperator(t)
			if test.reply != "" {
				operator.answer(test.reply)
			}
			if err := pairNative(operator.operatorEnv, cfg, test.yes); err != nil {
				t.Fatal(err)
			}
			if identityExists(cfg) {
				t.Fatal("identity.json was kept")
			}
			assertFilesKept(t, cfg, kept)
			if history := operator.service.history(); history != "stop,start" {
				t.Fatalf("service actions=%q", history)
			}
			output := operator.output.String()
			for _, want := range []string{
				"Removed the previous pairing (identity.json); the accession ledger is kept.",
				"Telrad keeps Relay relay-test until a company administrator revokes or replaces it in Telrad settings.",
				"Approve this Relay in your browser:\n\n  " + testPairingLink + "\n",
			} {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q:\n%s", want, output)
				}
			}
			if strings.Contains(output, "PRIVATE KEY") {
				t.Fatal("key material printed")
			}
		})
	}
}

func TestPairNativeReplacesUnreadableIdentity(t *testing.T) {
	cfg := testConfig(t, nil)
	kept := pairingDataDir(t, cfg)
	if err := os.WriteFile(cfg.dataPath(identityFileName), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	operator := newTestOperator(t)
	operator.service.onStart = func() { operator.status.set(&statusReport{State: "pairing", PairingLink: testPairingLink}) }
	if err := pairNative(operator.operatorEnv, cfg, false); err == nil || !identityExists(cfg) {
		t.Fatalf("unreadable identity replaced without confirmation: %v", err)
	}
	if err := pairNative(operator.operatorEnv, cfg, true); err != nil {
		t.Fatal(err)
	}
	if identityExists(cfg) || !strings.Contains(operator.output.String(), "cannot be used") || !strings.Contains(operator.output.String(), testPairingLink) {
		t.Fatalf("output=%q", operator.output.String())
	}
	assertFilesKept(t, cfg, kept)
}

func TestPairNativeReportsMissingLink(t *testing.T) {
	operator, cfg, kept := pairedOperator(t)
	operator.service.onStart = func() {
		operator.status.set(&statusReport{State: "pairing", PairingError: "enrolment request failed: dns_failure"})
	}
	err := pairNative(operator.operatorEnv, cfg, true)
	if err == nil || !strings.Contains(err.Error(), "run telrad later") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(operator.output.String(), "pairing problem: enrolment request failed: dns_failure") {
		t.Fatalf("output=%q", operator.output.String())
	}
	if operator.status.reads < 2 {
		t.Fatalf("status read %d times", operator.status.reads)
	}
	assertFilesKept(t, cfg, kept)
}

func TestPairNativeUnpaired(t *testing.T) {
	linkReport := &statusReport{State: "pairing", PairingLink: testPairingLink}
	for _, test := range []struct {
		name    string
		running bool
		report  *statusReport
		actions string
	}{
		{"shows the current link", true, linkReport, ""},
		{"restarts on a pairing problem", true, &statusReport{State: "pairing", PairingError: "enrolment failed: http_503"}, "restart"},
		{"restarts on an expired certificate", true, &statusReport{State: "pairing", RelayID: "relay-old", RenewalError: certificateExpiredText}, "restart"},
		{"starts a stopped service", false, nil, "start"},
		{"waits for a starting service", true, &statusReport{State: "pairing"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(t, nil)
			kept := pairingDataDir(t, cfg)
			operator := newTestOperator(t)
			operator.service.isRunning = test.running
			operator.status.set(test.report)
			operator.service.onStart = func() { operator.status.set(linkReport) }
			if test.actions == "" && test.report != nil && test.report.PairingLink == "" {
				// The starting service publishes its link on a later read.
				operator.sleep = func(time.Duration) { operator.status.set(linkReport) }
			}
			if err := pairNative(operator.operatorEnv, cfg, false); err != nil {
				t.Fatal(err)
			}
			if history := operator.service.history(); history != test.actions {
				t.Fatalf("service actions=%q", history)
			}
			if !strings.Contains(operator.output.String(), testPairingLink) {
				t.Fatalf("output=%q", operator.output.String())
			}
			assertFilesKept(t, cfg, kept)
		})
	}
}

func TestPairNativeRequiresElevation(t *testing.T) {
	operator, cfg, _ := pairedOperator(t)
	operator.elevated = func() bool { return false }
	if err := pairNative(operator.operatorEnv, cfg, true); err == nil || !identityExists(cfg) || operator.service.history() != "" {
		t.Fatalf("unelevated pair: %v", err)
	}
}

func useContainer(t *testing.T) {
	t.Helper()
	previous := distribution
	distribution = "docker"
	t.Cleanup(func() { distribution = previous })
}

func TestPairContainer(t *testing.T) {
	useContainer(t)
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	cfg.DataDir = filepath.Join(cfg.DataDir, "volume")
	t.Setenv(pairingTokenVariable, fake.token)
	var out bytes.Buffer
	if err := pairCommand(cfg, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Paired relay relay-token") || strings.Contains(out.String(), "Replaced") {
		t.Fatalf("out=%q", out.String())
	}
	if _, present := os.LookupEnv(pairingTokenVariable); present {
		t.Fatal("pairing token left in the environment")
	}
}

func TestPairContainerReplacesOnlyAfterIssue(t *testing.T) {
	useContainer(t)
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	pairedStore(t, cfg, pki, telradEndpoints{Host: "127.0.0.1", DicomPort: 1, HL7Port: 1, ReportPort: 1})
	kept := pairingDataDir(t, cfg)
	original, _ := os.ReadFile(cfg.dataPath(identityFileName))
	unchanged := func() {
		t.Helper()
		if current, _ := os.ReadFile(cfg.dataPath(identityFileName)); !bytes.Equal(current, original) {
			t.Fatal("identity.json changed")
		}
	}

	// Without --yes a paired volume is refused.
	t.Setenv(pairingTokenVariable, fake.token)
	var out bytes.Buffer
	err := pairCommand(cfg, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "already paired (Relay relay-test") || !strings.Contains(err.Error(), "pair --yes") {
		t.Fatalf("err=%v", err)
	}
	unchanged()

	// A refused token leaves the current pairing in place.
	t.Setenv(pairingTokenVariable, "wrong-token-0123456789")
	err = pairCommand(cfg, []string{"--yes"}, &out)
	if err == nil || !strings.Contains(err.Error(), "pairing token was refused") || !strings.Contains(err.Error(), "Relay relay-test) is unchanged") {
		t.Fatalf("err=%v", err)
	}
	unchanged()

	// A missing token is refused before anything changes.
	t.Setenv(pairingTokenVariable, "")
	if err := pairCommand(cfg, []string{"--yes"}, &out); err == nil || !strings.Contains(err.Error(), pairingTokenVariable) {
		t.Fatalf("err=%v", err)
	}
	unchanged()

	// With --yes and a valid token the new identity replaces the old one.
	t.Setenv(pairingTokenVariable, fake.token)
	out.Reset()
	if err := pairCommand(cfg, []string{"--yes"}, &out); err != nil {
		t.Fatal(err)
	}
	store, err := openIdentity(cfg)
	if err != nil || store.relayID() != "relay-token" {
		t.Fatalf("stored relay %q: %v", store.relayID(), err)
	}
	for _, want := range []string{"Paired relay relay-token", "Replaced Relay relay-test.", "Restart the Relay container"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q: %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), fake.token) {
		t.Fatal("token printed")
	}
	assertFilesKept(t, cfg, kept)
	assertPrivateFileMode(t, cfg.dataPath(identityFileName))
}

func TestPairArguments(t *testing.T) {
	cfg := testConfig(t, nil)
	for _, args := range [][]string{{"extra"}, {"--force"}} {
		if err := pairCommand(cfg, args, &bytes.Buffer{}); err == nil {
			t.Fatalf("pair %v accepted", args)
		}
	}
}
