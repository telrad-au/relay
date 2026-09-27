package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExecuteVersionHelpAndUnknown(t *testing.T) {
	var out bytes.Buffer
	if err := execute([]string{"version"}, &out); err != nil || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: %v %q", err, out.String())
	}
	out.Reset()
	if err := execute([]string{"help"}, &out); err != nil || !strings.Contains(out.String(), "telrad status") {
		t.Fatalf("help: %v", err)
	}
	if err := execute([]string{"--config", filepath.Join(t.TempDir(), "relay.json"), "bogus"}, &out); err == nil {
		t.Fatal("unknown command accepted")
	}
	if err := execute([]string{"status", "extra"}, &out); err == nil {
		t.Fatal("extra argument accepted")
	}
}

func TestStatusCommandReportsStoppedService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	address := listener.Addr().String()
	listener.Close()
	if err := os.WriteFile(path, []byte(`{"schemaVersion":6,"reportHost":"ris.local","statusAddress":"`+address+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := execute([]string{"--config", path, "status"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err=%v", err)
	}
	var out bytes.Buffer
	if err := execute([]string{"--config", path, "ready"}, &out); !errors.Is(err, errNotReady) || !strings.Contains(out.String(), "unreachable") {
		t.Fatalf("ready with stopped service: %v %q", err, out.String())
	}
}

func TestStatusEndpointAndReadiness(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.status.start(ctx); err != nil {
		t.Fatal(err)
	}
	address := r.status.listener.Addr().String()
	ready := func() int {
		response, err := http.Get("http://" + address + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if code := ready(); code != http.StatusServiceUnavailable {
		t.Fatalf("ready before listeners: %d", code)
	}
	var check bytes.Buffer
	if err := checkReady(address, &check); !errors.Is(err, errNotReady) || !strings.Contains(check.String(), `"reason":"listeners not bound"`) {
		t.Fatalf("telrad ready before listeners: %v %q", err, check.String())
	}
	r.status.setListeners(true, true)
	startPickup(t, r)
	port.accept(t)
	waitFor(t, func() bool { return ready() == http.StatusOK })
	check.Reset()
	if err := checkReady(address, &check); err != nil || strings.TrimSpace(check.String()) != "ready" {
		t.Fatalf("telrad ready: %v %q", err, check.String())
	}
	report, err := fetchStatus(address)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "ready" || !report.Paired || report.RelayID != "relay-test" || report.CertificateNotAfter == nil || report.Telrad.Host != "127.0.0.1" {
		t.Fatalf("report=%+v", report)
	}
	var out bytes.Buffer
	printStatus(&out, report)
	for _, want := range []string{"state: ready", "relay: relay-test", "report pickup: connected=true", "ledger entries: 0"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, out.String())
		}
	}
	raw, _ := http.Get("http://" + address + "/status")
	var generic map[string]any
	_ = json.NewDecoder(raw.Body).Decode(&generic)
	raw.Body.Close()
	if raw.Header.Get("Cache-Control") != "no-store" || generic["state"] != "ready" {
		t.Fatal("status response headers or body wrong")
	}
	// The CLI status output for an unpaired relay shows the link.
	unpaired := &statusReport{State: "pairing", PairingLink: "https://app.example.invalid/approve/x"}
	out.Reset()
	printStatus(&out, unpaired)
	if !strings.Contains(out.String(), "https://app.example.invalid/approve/x") {
		t.Fatal("pairing link not printed")
	}
}

// TestRunRelayEndToEnd starts the whole service against fake Telrad listeners,
// sends an order through it, then receives the report for that order.
func TestRunRelayEndToEnd(t *testing.T) {
	pki := newTestPKI(t)
	dicom := pki.mutualTLSListener(t)
	echoServer(t, dicom)
	hl7 := pki.mutualTLSListener(t)
	hl7Responder(t, hl7, func(message []byte) []byte { return ackFor(message, "AA") })
	port := newReportPort(t, pki)
	receiver, received := startReceiver(t, func(message []byte) []byte { return ackFor(message, "AA") })

	cfg := testConfig(t, pki)
	cfg.ReportPort = listenerPort(t, receiver)
	statusListener, _ := net.Listen("tcp", "127.0.0.1:0")
	cfg.StatusAddress = statusListener.Addr().String()
	statusListener.Close()
	for _, name := range []string{"dicom", "hl7"} {
		probe, _ := net.Listen("tcp", "127.0.0.1:0")
		if name == "dicom" {
			cfg.DicomPort = listenerPort(t, probe)
		} else {
			cfg.HL7Port = listenerPort(t, probe)
		}
		probe.Close()
	}
	cfg.ListenAddress = "127.0.0.1"
	pairedStore(t, cfg, pki, telradEndpoints{Host: "127.0.0.1", DicomPort: listenerPort(t, dicom), HL7Port: listenerPort(t, hl7), ReportPort: listenerPort(t, port.listener)})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runRelay(ctx, cfg) }()
	telrad := port.accept(t)
	waitFor(t, func() bool {
		report, err := fetchStatus(cfg.StatusAddress)
		return err == nil && report.State == "ready"
	})

	clinic, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.HL7Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer clinic.Close()
	ack := mllpExchange(t, clinic, bufioReader(clinic), []byte(testOrder))
	if !bytes.HasSuffix(ack, []byte("MSA|AA|MSG0001\r")) {
		t.Fatalf("order ack=%q", ack)
	}
	reportAck := mllpExchange(t, telrad, bufioReader(telrad), []byte(testReport))
	if !bytes.Equal(reportAck, ackFor([]byte(testReport), "AA")) || received.count() != 1 {
		t.Fatalf("report ack=%q received=%d", reportAck, received.count())
	}
	pacs, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.DicomPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer pacs.Close()
	_, _ = pacs.Write([]byte("ping"))
	buffer := make([]byte, 4)
	_ = pacs.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := pacs.Read(buffer); err != nil || string(buffer) != "ping" {
		t.Fatalf("dicom echo=%q err=%v", buffer, err)
	}
	report, _ := fetchStatus(cfg.StatusAddress)
	if report.LedgerEntries != 1 || report.Reports.Delivered != 1 {
		t.Fatalf("status=%+v", report)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("service did not stop")
	}
}

func TestRunRelayInContainerRequiresTokenWhenUnpaired(t *testing.T) {
	previous := distribution
	distribution = "docker"
	t.Cleanup(func() { distribution = previous })
	cfg := testConfig(t, nil)
	cfg.StatusAddress = "127.0.0.1:0"
	t.Setenv(pairingTokenVariable, "")
	err := runRelay(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), pairingTokenVariable) {
		t.Fatalf("err=%v", err)
	}
}

func TestEnrollCommandPairsContainer(t *testing.T) {
	previous := distribution
	distribution = "docker"
	t.Cleanup(func() { distribution = previous })
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	t.Setenv(pairingTokenVariable, fake.token)
	var out bytes.Buffer
	if err := enrollCommand(cfg, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Paired relay relay-token") {
		t.Fatalf("out=%q", out.String())
	}
	if _, present := os.LookupEnv(pairingTokenVariable); present {
		t.Fatal("pairing token left in the environment")
	}
	out.Reset()
	if err := enrollCommand(cfg, &out); err != nil || !strings.Contains(out.String(), "already paired") {
		t.Fatalf("second enroll: %v %q", err, out.String())
	}
}
