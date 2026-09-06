package main

import "testing"

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
