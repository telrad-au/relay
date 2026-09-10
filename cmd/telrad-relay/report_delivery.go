package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"
)

type reportMessage struct {
	Type             string    `json:"type"`
	DeliveryID       string    `json:"deliveryId"`
	Token            string    `json:"token"`
	MessageControlID string    `json:"messageControlId"`
	Payload          string    `json:"payload"`
	PayloadSHA256    string    `json:"payloadSha256"`
	ClaimExpiresAt   time.Time `json:"claimExpiresAt"`
}

type reportResult struct {
	Token   string `json:"token"`
	Outcome string `json:"outcome"`
	AckCode string `json:"ackCode,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Report delivery is deliberately stateless: the cloud owns retries and the RIS
// must handle a retransmission of the exact message without duplicate effects.
func deliverReport(ctx context.Context, cfg *config, report reportMessage) reportResult {
	failure := func(code, ack string) reportResult {
		return reportResult{Token: report.Token, Outcome: "failed", Error: code, AckCode: ack}
	}
	if retrievalEnabledLocal(cfg) && !safeRetrievalReport(report.Payload) {
		return failure("invalid_report", "")
	}
	digest := sha256.Sum256([]byte(report.Payload))
	controlID, err := hl7MessageControlID(report.Payload)
	if !validOpaqueID(report.DeliveryID) || !validOpaqueID(report.Token) || hex.EncodeToString(digest[:]) != report.PayloadSHA256 || err != nil || controlID != report.MessageControlID {
		return failure("invalid_report", "")
	}
	ack, err := sendMLLP(ctx, cfg.ReportHost, cfg.ReportPort, report.Payload)
	if err == nil && ack == "AA" {
		return reportResult{Token: report.Token, Outcome: "accepted", AckCode: ack}
	}
	if ack != "" {
		return failure("clinic_rejected", ack)
	}
	return failure(safeNetworkError(err).Error(), "")
}

func sendMLLP(ctx context.Context, host string, port int, message string) (string, error) {
	controlID, err := hl7MessageControlID(message)
	if err != nil {
		return "", err
	}
	connection, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return "", err
	}
	defer connection.Close()
	deadline := time.Now().Add(20 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	_ = connection.SetDeadline(deadline)
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancel()
	frame := append([]byte{0x0b}, []byte(message)...)
	frame = append(frame, 0x1c, 0x0d)
	if _, err := connection.Write(frame); err != nil {
		return "", err
	}
	data, err := readMLLPFrame(connection, 1024*1024)
	if err != nil {
		return "", err
	}
	return parseMLLPAcknowledgement(data, controlID)
}

func parseMLLPAcknowledgement(data []byte, controlID string) (string, error) {
	if len(data) < 3 || data[0] != 0x0b || data[len(data)-2] != 0x1c || data[len(data)-1] != 0x0d || bytes.Contains(data[1:len(data)-2], []byte{0x1c, 0x0d}) {
		return "", errors.New("clinic returned malformed MLLP acknowledgement framing")
	}
	code, receivedControlID, err := parseHL7Acknowledgement(data[1 : len(data)-2])
	if err != nil || receivedControlID != controlID {
		return "", errors.New("clinic acknowledgement does not correlate with outbound MSH-10")
	}
	if code != "AA" {
		return code, fmt.Errorf("clinic returned HL7 %s", code)
	}
	return code, nil
}

func hl7MessageControlID(message string) (string, error) { return hl7ControlID([]byte(message)) }

// A compromised report channel must not return an ORM that a RIS could loop
// into its approved referral feed. This restriction applies to opted-in clinics.
func safeRetrievalReport(payload string) bool {
	if bytes.ContainsAny([]byte(payload), "\x0b\x1c") {
		return false
	}
	segments, err := referralSegments([]byte(payload))
	if err != nil {
		return false
	}
	kind := field(segments[0], 8)
	if kind != "ORU^R01" && kind != "ORU^R01^ORU_R01" {
		return false
	}
	for _, segment := range segments[1:] {
		if segment[0] == "MSH" {
			return false
		}
	}
	return true
}
