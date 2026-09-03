package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	reportLedgerRetention  = 30 * 24 * time.Hour
	reportLedgerMaxEntries = 10000
)

type reportDeliveryRecord struct {
	DeliveryID    string    `json:"deliveryId"`
	TokenSHA256   string    `json:"tokenSha256"`
	PayloadSHA256 string    `json:"payloadSha256"`
	ControlIDHash string    `json:"controlIdSha256"`
	LegacyControl string    `json:"controlId,omitempty"`
	State         string    `json:"state"`
	AckCode       string    `json:"ackCode,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type reportDeliveryLedger struct {
	mu      sync.Mutex
	path    string
	records map[string]reportDeliveryRecord
}

func openReportDeliveryLedger(configPath string) (*reportDeliveryLedger, error) {
	ledger := &reportDeliveryLedger{
		path:    filepath.Join(filepath.Dir(configPath), "report-delivery-ledger.json"),
		records: make(map[string]reportDeliveryRecord),
	}
	data, err := os.ReadFile(ledger.path)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &ledger.records); err != nil {
		return nil, fmt.Errorf("decode report delivery ledger: %w", err)
	}
	converted := false
	for key, record := range ledger.records {
		if record.ControlIDHash == "" && record.LegacyControl != "" {
			record.ControlIDHash = hashString(record.LegacyControl)
			record.LegacyControl = ""
			ledger.records[key] = record
			converted = true
		}
		if key == "" || key != record.DeliveryID || record.TokenSHA256 == "" || record.PayloadSHA256 == "" || record.ControlIDHash == "" || record.UpdatedAt.IsZero() {
			return nil, errors.New("report delivery ledger is invalid; administrator repair is required")
		}
		if record.State != "pending" && record.State != "accepted" && record.State != "rejected" {
			return nil, errors.New("report delivery ledger contains an unknown state")
		}
	}
	if converted {
		if err := atomicWriteJSON(ledger.path, ledger.records); err != nil {
			return nil, err
		}
	}
	return ledger, nil
}

func (ledger *reportDeliveryLedger) Begin(deliveryID, token, payloadHash, controlID string, now time.Time) (reportDeliveryRecord, bool, error) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if deliveryID == "" || token == "" || payloadHash == "" || controlID == "" || now.IsZero() {
		return reportDeliveryRecord{}, false, errors.New("report delivery ledger metadata is incomplete")
	}
	key := reportLedgerKey(deliveryID)
	if record, ok := ledger.records[key]; ok {
		if record.DeliveryID != deliveryID || record.TokenSHA256 != hashString(token) || record.PayloadSHA256 != payloadHash || record.ControlIDHash != hashString(controlID) {
			return reportDeliveryRecord{}, false, errors.New("delivery identifier was reused with different report metadata")
		}
		return record, false, nil
	}
	ledger.prune(now)
	if len(ledger.records) >= reportLedgerMaxEntries {
		return reportDeliveryRecord{}, false, errors.New("report delivery ledger is full; administrator review is required")
	}
	record := reportDeliveryRecord{
		DeliveryID: deliveryID, TokenSHA256: hashString(token), PayloadSHA256: payloadHash,
		ControlIDHash: hashString(controlID), State: "pending", UpdatedAt: now.UTC(),
	}
	ledger.records[key] = record
	if err := atomicWriteJSON(ledger.path, ledger.records); err != nil {
		delete(ledger.records, key)
		return reportDeliveryRecord{}, false, err
	}
	return record, true, nil
}

func (ledger *reportDeliveryLedger) Complete(deliveryID, token, state, ackCode string, now time.Time) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	key := reportLedgerKey(deliveryID)
	record, ok := ledger.records[key]
	if !ok || record.State != "pending" {
		return errors.New("report delivery has no pending ledger record")
	}
	if record.TokenSHA256 != hashString(token) {
		return errors.New("report delivery token does not match the pending ledger record")
	}
	if state != "accepted" && state != "rejected" {
		return errors.New("report delivery completion state is invalid")
	}
	record.State = state
	record.AckCode = ackCode
	record.UpdatedAt = now.UTC()
	ledger.records[key] = record
	return atomicWriteJSON(ledger.path, ledger.records)
}

func (ledger *reportDeliveryLedger) prune(now time.Time) {
	cutoff := now.Add(-reportLedgerRetention)
	keys := make([]string, 0, len(ledger.records))
	for key, record := range ledger.records {
		if record.State != "pending" && record.UpdatedAt.Before(cutoff) {
			delete(ledger.records, key)
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) < reportLedgerMaxEntries {
		return
	}
	sort.Slice(keys, func(i, j int) bool {
		return ledger.records[keys[i]].UpdatedAt.Before(ledger.records[keys[j]].UpdatedAt)
	})
	for _, key := range keys {
		if len(ledger.records) < reportLedgerMaxEntries {
			break
		}
		if ledger.records[key].State != "pending" {
			delete(ledger.records, key)
		}
	}
}

func reportLedgerKey(deliveryID string) string {
	return deliveryID
}

func hashString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
