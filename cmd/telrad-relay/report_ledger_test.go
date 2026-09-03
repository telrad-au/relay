package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportLedgerDurablyDeduplicatesAcceptedDelivery(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	ledger, err := openReportDeliveryLedger(configPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, fresh, err := ledger.Begin("delivery-1", "secret-token", "payload-hash", "control-1", now); err != nil || !fresh {
		t.Fatalf("begin delivery: fresh=%v error=%v", fresh, err)
	}
	if err := ledger.Complete("delivery-1", "secret-token", "accepted", "AA", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reloaded, err := openReportDeliveryLedger(configPath)
	if err != nil {
		t.Fatal(err)
	}
	record, fresh, err := reloaded.Begin("delivery-1", "secret-token", "payload-hash", "control-1", now.Add(2*time.Second))
	if err != nil || fresh || record.State != "accepted" || record.AckCode != "AA" {
		t.Fatalf("durable duplicate lookup = %#v, fresh=%v, error=%v", record, fresh, err)
	}
	contents, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "report-delivery-ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) == "" || strings.Contains(string(contents), "secret-token") {
		t.Fatal("ledger is empty or contains the plaintext delivery token")
	}
}

func TestReportLedgerPreservesAmbiguousCrashWindow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	ledger, err := openReportDeliveryLedger(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := ledger.Begin("delivery-2", "token", "payload", "control", time.Now()); err != nil || !fresh {
		t.Fatalf("begin delivery: fresh=%v error=%v", fresh, err)
	}
	reloaded, err := openReportDeliveryLedger(configPath)
	if err != nil {
		t.Fatal(err)
	}
	record, fresh, err := reloaded.Begin("delivery-2", "token", "payload", "control", time.Now())
	if err != nil || fresh || record.State != "pending" {
		t.Fatalf("crash-window lookup = %#v, fresh=%v, error=%v", record, fresh, err)
	}
	if _, _, err := reloaded.Begin("delivery-2", "different-token", "payload", "control", time.Now()); err == nil {
		t.Fatal("delivery identifier reuse with a different token was accepted")
	}
}

func TestParseMLLPAcknowledgementCorrelatesControlIDAndFraming(t *testing.T) {
	valid := []byte("\x0bMSH|^~\\&|RIS|CLINIC|TELRAD|CLOUD|20260101000000||ACK|ACK-1|P|2.5\rMSA|AA|control-1\x1c\x0d")
	if code, err := parseMLLPAcknowledgement(valid, "control-1"); err != nil || code != "AA" {
		t.Fatalf("valid ACK code=%q error=%v", code, err)
	}
	stale := []byte("\x0bMSA|AA|other-control\x1c\x0d")
	if _, err := parseMLLPAcknowledgement(stale, "control-1"); err == nil {
		t.Fatal("stale ACK was accepted")
	}
	if _, err := parseMLLPAcknowledgement([]byte("MSA|AA|control-1\r"), "control-1"); err == nil {
		t.Fatal("unframed ACK was accepted")
	}
	if code, err := parseMLLPAcknowledgement([]byte("\x0bMSA|AE|control-1\x1c\x0d"), "control-1"); code != "AE" || err == nil {
		t.Fatalf("NACK code=%q error=%v", code, err)
	}
}

func TestHL7MessageControlID(t *testing.T) {
	message := "MSH|^~\\&|TELRAD|CLOUD|RIS|CLINIC|20260101000000||ORU^R01|control-42|P|2.5\rPID|1"
	if got, err := hl7MessageControlID(message); err != nil || got != "control-42" {
		t.Fatalf("control ID=%q error=%v", got, err)
	}
	if _, err := hl7MessageControlID("PID|1"); err == nil {
		t.Fatal("message without MSH was accepted")
	}
}
