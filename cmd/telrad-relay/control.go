package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const maxControlMessageSize = maxCloudResponseBytes

type readyMessage struct {
	Type       string                    `json:"type"`
	SessionID  string                    `json:"sessionId"`
	Transports map[string]readyTransport `json:"transports"`
}

type readyTransport struct {
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
}

func superviseControl(ctx context.Context, cfg *config, configPath string, client *http.Client, provider *credentialProvider, credentialChanged <-chan struct{}, work *workDrainer, status *runtimeStatusManager) {
	backoff := time.Second
	for ctx.Err() == nil {
		sessionCtx, cancel := context.WithCancel(ctx)
		started := time.Now()
		established, errCh := connectControl(sessionCtx, cfg, configPath, client, provider, work, status)
		select {
		case <-ctx.Done():
			cancel()
			return
		case <-credentialChanged:
			cancel()
			if errCh != nil {
				<-errCh
			}
			status.SetControlConnected(false)
			continue
		case <-errCh:
			cancel()
			status.SetControlConnected(false)
		}
		backoff = nextReconnectBackoff(backoff, established, time.Since(started))
		delay := jitterReconnectDelay(backoff)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-credentialChanged:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func connectControl(ctx context.Context, cfg *config, configPath string, client *http.Client, provider *credentialProvider, work *workDrainer, status *runtimeStatusManager) (bool, <-chan error) {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+provider.Current())
	headers.Set("X-Telrad-Protocol-Version", protocolVersion)
	connection, response, err := websocket.Dial(ctx, cfg.ControlURL, &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if response != nil {
			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
				status.SetAuthenticationAttention(true)
			}
			_ = response.Body.Close()
		}
		failed := make(chan error, 1)
		failed <- err
		return false, failed
	}
	connection.SetReadLimit(maxControlMessageSize)
	hostname, _ := os.Hostname()
	hello := map[string]any{
		"type": "hello", "agentVersion": version, "platform": relayPlatform(), "hostname": hostname,
		"capabilities": map[string]bool{"dicom": true, "hl7": true, "reportDelivery": true, "httpsIngest": true},
	}
	if err := writeJSON(ctx, connection, hello); err != nil {
		connection.CloseNow()
		failed := make(chan error, 1)
		failed <- err
		return false, failed
	}
	var ready readyMessage
	if err := readJSON(ctx, connection, &ready); err != nil || validateReady(cfg, ready) != nil {
		connection.CloseNow()
		failed := make(chan error, 1)
		failed <- errors.New("control ready message is invalid")
		return false, failed
	}
	status.SetControlConnected(true)
	errCh := make(chan error, 1)
	go func() {
		defer connection.CloseNow()
		errCh <- controlLoop(ctx, cfg, configPath, connection, work)
	}()
	return true, errCh
}

func validateReady(cfg *config, ready readyMessage) error {
	if ready.Type != "ready" || !validOpaqueID(ready.SessionID) {
		return errors.New("missing session")
	}
	expected := map[string]readyTransport{
		"dicom": {URL: cfg.DicomURL, ContentType: "application/dicom"},
		"hl7":   {URL: cfg.HL7URL, ContentType: "application/hl7-v2"},
	}
	if len(ready.Transports) != len(expected) {
		return errors.New("unexpected transports")
	}
	for name, value := range expected {
		if ready.Transports[name] != value {
			return errors.New("transport mismatch")
		}
	}
	return nil
}

func controlLoop(ctx context.Context, cfg *config, configPath string, connection *websocket.Conn, work *workDrainer) error {
	var writeMu sync.Mutex
	ledger, err := openReportDeliveryLedger(configPath)
	if err != nil {
		return err
	}
	for {
		var message map[string]any
		if err := readJSON(ctx, connection, &message); err != nil {
			return err
		}
		if message["type"] != "report" {
			continue
		}
		deliveryID, _ := message["deliveryId"].(string)
		token, _ := message["token"].(string)
		payload, _ := message["payload"].(string)
		expectedHash, _ := message["payloadSha256"].(string)
		if !work.Start() {
			return writeJSON(ctx, connection, map[string]string{"type": "reportFail", "deliveryId": deliveryID, "token": token, "error": "relay_draining"})
		}
		func() {
			defer work.Done()
			response := deliverReport(ctx, cfg, ledger, deliveryID, token, payload, expectedHash)
			writeMu.Lock()
			err = writeJSON(ctx, connection, response)
			writeMu.Unlock()
		}()
		if err != nil {
			return err
		}
	}
}

func deliverReport(ctx context.Context, cfg *config, ledger *reportDeliveryLedger, deliveryID, token, payload, expectedHash string) map[string]string {
	failure := func(code, ackCode string) map[string]string {
		return map[string]string{"type": "reportFail", "deliveryId": deliveryID, "token": token, "ackCode": ackCode, "error": code}
	}
	hash := sha256.Sum256([]byte(payload))
	controlID, err := hl7MessageControlID(payload)
	if !validOpaqueID(deliveryID) || !validOpaqueID(token) || hex.EncodeToString(hash[:]) != expectedHash || err != nil {
		return failure("invalid_report", "AE")
	}
	record, fresh, err := ledger.Begin(deliveryID, token, expectedHash, controlID, time.Now())
	if err != nil {
		return failure("ledger_error", "AE")
	}
	if !fresh {
		if record.State == "accepted" {
			return map[string]string{"type": "reportAck", "deliveryId": deliveryID, "token": token, "ackCode": "AA"}
		}
		return failure("delivery_reconciliation_required", record.AckCode)
	}
	ackCode, sendErr := sendMLLP(ctx, cfg.ReportHost, cfg.ReportPort, payload)
	if sendErr == nil && ackCode == "AA" {
		if ledger.Complete(deliveryID, token, "accepted", ackCode, time.Now()) != nil {
			return failure("ledger_error", "AE")
		}
		return map[string]string{"type": "reportAck", "deliveryId": deliveryID, "token": token, "ackCode": ackCode}
	}
	if ackCode != "" {
		_ = ledger.Complete(deliveryID, token, "rejected", ackCode, time.Now())
	}
	return failure("clinic_rejected", ackCode)
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
	_ = connection.SetDeadline(time.Now().Add(20 * time.Second))
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

func readJSON(ctx context.Context, connection *websocket.Conn, value any) error {
	_, data, err := connection.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSON(ctx context.Context, connection *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, data)
}
