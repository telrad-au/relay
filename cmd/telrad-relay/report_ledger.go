package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	reportLedgerRetention      = 30 * 24 * time.Hour
	reportLedgerMaxEntries     = 10000
	reportLedgerPruneBatchSize = 32
	reportLedgerOpenTimeout    = time.Second
	reportLedgerSchemaVersion  = uint64(1)
	reportLedgerFilename       = "report-delivery-ledger.db"
	reportLedgerLegacyFilename = "report-delivery-ledger.json"
	reportLedgerBackupFilename = "report-delivery-ledger.pre-bbolt.json"
	reportLedgerLegacyMarker   = "migrated to report-delivery-ledger.db; older Relay versions must not use this ledger\n"
)

var (
	reportLedgerRecordsBucket  = []byte("records")
	reportLedgerTerminalBucket = []byte("terminal-by-time")
	reportLedgerMetadataBucket = []byte("metadata")
	reportLedgerSchemaKey      = []byte("schema-version")
	reportLedgerCountKey       = []byte("record-count")
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

type reportDeliveryStore interface {
	Begin(deliveryID, token, payloadHash, controlID string, now time.Time) (reportDeliveryRecord, bool, error)
	Complete(deliveryID, token, state, ackCode string, now time.Time) error
}

type reportDeliveryLedger struct {
	mu                sync.Mutex
	legacyPath        string
	legacyInvalidated bool
	db                *bolt.DB
	update            func(func(*bolt.Tx) error) error
}

func openReportDeliveryLedger(configPath string) (*reportDeliveryLedger, error) {
	directory := filepath.Dir(configPath)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, reportLedgerFilename)
	legacyPath := filepath.Join(directory, reportLedgerLegacyFilename)
	if err := prepareReportLedgerDatabase(directory, path, legacyPath); err != nil {
		return nil, err
	}

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: reportLedgerOpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("open report delivery ledger: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0600); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	legacyInvalidated, err := legacyReportLedgerInvalidated(legacyPath)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	ledger := &reportDeliveryLedger{
		legacyPath: legacyPath, legacyInvalidated: legacyInvalidated,
		db: db, update: db.Update,
	}
	if err := ledger.validateDatabase(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return ledger, nil
}

func legacyReportLedgerInvalidated(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		return false, errors.New("legacy report delivery ledger path is a directory")
	}
	if info.Size() != int64(len(reportLedgerLegacyMarker)) {
		return false, nil
	}
	marker, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return string(marker) == reportLedgerLegacyMarker, nil
}

func prepareReportLedgerDatabase(directory, path, legacyPath string) error {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return errors.New("report delivery ledger path is a directory")
		}
		if info.Size() == 0 {
			return errors.New("report delivery ledger is empty; administrator repair is required")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	legacyInfo, err := os.Stat(legacyPath)
	if errors.Is(err, os.ErrNotExist) {
		return createReportLedgerDatabase(path, nil)
	}
	if err != nil {
		return err
	}
	if legacyInfo.IsDir() {
		return errors.New("legacy report delivery ledger path is a directory")
	}
	legacy, err := readLegacyReportLedger(legacyPath)
	if err != nil {
		return err
	}
	if err := atomicWriteJSON(filepath.Join(directory, reportLedgerBackupFilename), legacy); err != nil {
		return fmt.Errorf("back up legacy report delivery ledger: %w", err)
	}
	if err := atomicWriteJSON(legacyPath, legacy); err != nil {
		return fmt.Errorf("normalize legacy report delivery ledger: %w", err)
	}
	if err := createReportLedgerDatabase(path, legacy); err != nil {
		return fmt.Errorf("migrate report delivery ledger: %w", err)
	}
	return nil
}

func (ledger *reportDeliveryLedger) Close() error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return ledger.db.Close()
}

func (ledger *reportDeliveryLedger) invalidateLegacyLedger() error {
	if ledger.legacyInvalidated {
		return nil
	}
	if err := atomicWriteFile(ledger.legacyPath, []byte(reportLedgerLegacyMarker), 0600); err != nil {
		return fmt.Errorf("invalidate legacy report delivery ledger: %w", err)
	}
	ledger.legacyInvalidated = true
	return nil
}

func (ledger *reportDeliveryLedger) Begin(deliveryID, token, payloadHash, controlID string, now time.Time) (reportDeliveryRecord, bool, error) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if deliveryID == "" || token == "" || payloadHash == "" || controlID == "" || now.IsZero() {
		return reportDeliveryRecord{}, false, errors.New("report delivery ledger metadata is incomplete")
	}
	record, found, err := reportLedgerRecord(ledger.db, deliveryID)
	if err != nil {
		return reportDeliveryRecord{}, false, err
	}
	if found {
		if record.TokenSHA256 != hashString(token) || record.PayloadSHA256 != payloadHash || record.ControlIDHash != hashString(controlID) {
			return reportDeliveryRecord{}, false, errors.New("delivery identifier was reused with different report metadata")
		}
		return record, false, nil
	}

	record = reportDeliveryRecord{
		DeliveryID: deliveryID, TokenSHA256: hashString(token), PayloadSHA256: payloadHash,
		ControlIDHash: hashString(controlID), State: "pending", UpdatedAt: now.UTC(),
	}
	if err := ledger.invalidateLegacyLedger(); err != nil {
		return reportDeliveryRecord{}, false, err
	}
	err = ledger.update(func(tx *bolt.Tx) error {
		records := tx.Bucket(reportLedgerRecordsBucket)
		terminal := tx.Bucket(reportLedgerTerminalBucket)
		metadata := tx.Bucket(reportLedgerMetadataBucket)
		if records == nil || terminal == nil || metadata == nil {
			return errors.New("report delivery ledger schema is missing")
		}
		if records.Get([]byte(deliveryID)) != nil {
			return errors.New("report delivery ledger changed during begin")
		}
		count, err := reportLedgerCount(metadata)
		if err != nil {
			return err
		}
		count, err = pruneExpiredReportRecords(records, terminal, count, now.Add(-reportLedgerRetention), reportLedgerPruneBatchSize)
		if err != nil {
			return err
		}
		if count >= reportLedgerMaxEntries {
			oldest, _ := terminal.Cursor().First()
			if oldest == nil {
				return errors.New("report delivery ledger is full; administrator review is required")
			}
			if err := deleteIndexedReportRecord(records, terminal, bytes.Clone(oldest)); err != nil {
				return err
			}
			count--
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := records.Put([]byte(deliveryID), encoded); err != nil {
			return err
		}
		return putReportLedgerCount(metadata, count+1)
	})
	if err != nil {
		return reportDeliveryRecord{}, false, err
	}
	return record, true, nil
}

func (ledger *reportDeliveryLedger) Complete(deliveryID, token, state, ackCode string, now time.Time) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if now.IsZero() {
		return errors.New("report delivery completion time is missing")
	}
	if state != "accepted" && state != "rejected" {
		return errors.New("report delivery completion state is invalid")
	}
	record, found, err := reportLedgerRecord(ledger.db, deliveryID)
	if err != nil {
		return err
	}
	if !found || record.State != "pending" {
		return errors.New("report delivery has no pending ledger record")
	}
	if record.TokenSHA256 != hashString(token) {
		return errors.New("report delivery token does not match the pending ledger record")
	}
	if err := ledger.invalidateLegacyLedger(); err != nil {
		return err
	}
	return ledger.update(func(tx *bolt.Tx) error {
		records := tx.Bucket(reportLedgerRecordsBucket)
		terminal := tx.Bucket(reportLedgerTerminalBucket)
		if records == nil || terminal == nil {
			return errors.New("report delivery ledger schema is missing")
		}
		data := records.Get([]byte(deliveryID))
		if data == nil {
			return errors.New("report delivery has no pending ledger record")
		}
		record, err := decodeStoredReportRecord(deliveryID, data)
		if err != nil {
			return err
		}
		if record.State != "pending" {
			return errors.New("report delivery has no pending ledger record")
		}
		if record.TokenSHA256 != hashString(token) {
			return errors.New("report delivery token does not match the pending ledger record")
		}
		record.State = state
		record.AckCode = ackCode
		record.UpdatedAt = now.UTC()
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := records.Put([]byte(deliveryID), encoded); err != nil {
			return err
		}
		return terminal.Put(reportTerminalIndexKey(record), nil)
	})
}

func (ledger *reportDeliveryLedger) validateDatabase() error {
	return ledger.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(reportLedgerRecordsBucket) == nil || tx.Bucket(reportLedgerTerminalBucket) == nil {
			return errors.New("report delivery ledger schema is missing")
		}
		metadata := tx.Bucket(reportLedgerMetadataBucket)
		if metadata == nil {
			return errors.New("report delivery ledger metadata is missing")
		}
		version := metadata.Get(reportLedgerSchemaKey)
		if len(version) != 8 || binary.BigEndian.Uint64(version) != reportLedgerSchemaVersion {
			return errors.New("report delivery ledger schema version is unsupported")
		}
		count, err := reportLedgerCount(metadata)
		if err != nil {
			return err
		}
		if count > reportLedgerMaxEntries {
			return errors.New("report delivery ledger exceeds its record limit")
		}
		return nil
	})
}

func createReportLedgerDatabase(path string, initial map[string]reportDeliveryRecord) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".report-delivery-ledger-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	defer os.Remove(temporaryPath)
	db, err := bolt.Open(temporaryPath, 0600, &bolt.Options{Timeout: reportLedgerOpenTimeout})
	if err != nil {
		return err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		records, err := tx.CreateBucket(reportLedgerRecordsBucket)
		if err != nil {
			return err
		}
		terminal, err := tx.CreateBucket(reportLedgerTerminalBucket)
		if err != nil {
			return err
		}
		metadata, err := tx.CreateBucket(reportLedgerMetadataBucket)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(initial))
		for key := range initial {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			record := initial[key]
			encoded, err := json.Marshal(record)
			if err != nil {
				return err
			}
			if err := records.Put([]byte(key), encoded); err != nil {
				return err
			}
			if record.State != "pending" {
				if err := terminal.Put(reportTerminalIndexKey(record), nil); err != nil {
					return err
				}
			}
		}
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], reportLedgerSchemaVersion)
		if err := metadata.Put(reportLedgerSchemaKey, version[:]); err != nil {
			return err
		}
		return putReportLedgerCount(metadata, uint64(len(initial)))
	})
	closeErr := db.Close()
	if err != nil {
		return errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	if err := syncDirectory(directory); err != nil {
		return err
	}
	return nil
}

func readLegacyReportLedger(path string) (map[string]reportDeliveryRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if string(data) == reportLedgerLegacyMarker {
		return nil, errors.New("report delivery ledger migration marker exists but the transactional ledger is missing")
	}
	var records map[string]reportDeliveryRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decode report delivery ledger: %w", err)
	}
	for key, record := range records {
		normalized, err := normalizeLegacyReportRecord(key, record)
		if err != nil {
			return nil, err
		}
		records[key] = normalized
	}
	if len(records) > reportLedgerMaxEntries {
		return nil, errors.New("report delivery ledger exceeds its record limit")
	}
	return records, nil
}

func normalizeLegacyReportRecord(key string, record reportDeliveryRecord) (reportDeliveryRecord, error) {
	if record.LegacyControl != "" {
		legacyHash := hashString(record.LegacyControl)
		if record.ControlIDHash != "" && record.ControlIDHash != legacyHash {
			return reportDeliveryRecord{}, errors.New("report delivery ledger has conflicting control metadata")
		}
		record.ControlIDHash = legacyHash
		record.LegacyControl = ""
	}
	if err := validateReportRecord(key, record); err != nil {
		return reportDeliveryRecord{}, err
	}
	return record, nil
}

func validateReportRecord(key string, record reportDeliveryRecord) error {
	if key == "" || key != record.DeliveryID || record.TokenSHA256 == "" || record.PayloadSHA256 == "" || record.ControlIDHash == "" || record.UpdatedAt.IsZero() {
		return errors.New("report delivery ledger is invalid; administrator repair is required")
	}
	if record.State != "pending" && record.State != "accepted" && record.State != "rejected" {
		return errors.New("report delivery ledger contains an unknown state")
	}
	if record.LegacyControl != "" {
		return errors.New("report delivery ledger contains plaintext control metadata")
	}
	return nil
}

func decodeStoredReportRecord(key string, data []byte) (reportDeliveryRecord, error) {
	var record reportDeliveryRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return reportDeliveryRecord{}, errors.New("report delivery ledger record is invalid")
	}
	if err := validateReportRecord(key, record); err != nil {
		return reportDeliveryRecord{}, err
	}
	return record, nil
}

func reportLedgerRecord(db *bolt.DB, deliveryID string) (reportDeliveryRecord, bool, error) {
	var record reportDeliveryRecord
	found := false
	err := db.View(func(tx *bolt.Tx) error {
		records := tx.Bucket(reportLedgerRecordsBucket)
		if records == nil {
			return errors.New("report delivery ledger schema is missing")
		}
		data := records.Get([]byte(deliveryID))
		if data == nil {
			return nil
		}
		decoded, err := decodeStoredReportRecord(deliveryID, data)
		if err != nil {
			return err
		}
		record = decoded
		found = true
		return nil
	})
	return record, found, err
}

func reportLedgerCount(metadata *bolt.Bucket) (uint64, error) {
	encoded := metadata.Get(reportLedgerCountKey)
	if len(encoded) != 8 {
		return 0, errors.New("report delivery ledger count is invalid")
	}
	return binary.BigEndian.Uint64(encoded), nil
}

func putReportLedgerCount(metadata *bolt.Bucket, count uint64) error {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], count)
	return metadata.Put(reportLedgerCountKey, encoded[:])
}

func reportTerminalIndexKey(record reportDeliveryRecord) []byte {
	key := make([]byte, 12+len(record.DeliveryID))
	orderedSeconds := uint64(record.UpdatedAt.Unix()) ^ (uint64(1) << 63)
	binary.BigEndian.PutUint64(key[:8], orderedSeconds)
	binary.BigEndian.PutUint32(key[8:12], uint32(record.UpdatedAt.Nanosecond()))
	copy(key[12:], record.DeliveryID)
	return key
}

func pruneExpiredReportRecords(records, terminal *bolt.Bucket, count uint64, cutoff time.Time, limit int) (uint64, error) {
	for range limit {
		indexKey, _ := terminal.Cursor().First()
		if indexKey == nil {
			break
		}
		if len(indexKey) <= 12 {
			return 0, errors.New("report delivery ledger retention index is invalid")
		}
		orderedSeconds := binary.BigEndian.Uint64(indexKey[:8]) ^ (uint64(1) << 63)
		indexedAt := time.Unix(int64(orderedSeconds), int64(binary.BigEndian.Uint32(indexKey[8:12])))
		if !indexedAt.Before(cutoff) {
			break
		}
		if count == 0 {
			return 0, errors.New("report delivery ledger count is invalid")
		}
		if err := deleteIndexedReportRecord(records, terminal, bytes.Clone(indexKey)); err != nil {
			return 0, err
		}
		count--
	}
	return count, nil
}

func deleteIndexedReportRecord(records, terminal *bolt.Bucket, indexKey []byte) error {
	if len(indexKey) <= 12 {
		return errors.New("report delivery ledger retention index is invalid")
	}
	deliveryID := string(indexKey[12:])
	data := records.Get([]byte(deliveryID))
	if data == nil {
		return errors.New("report delivery ledger retention index is inconsistent")
	}
	record, err := decodeStoredReportRecord(deliveryID, data)
	if err != nil {
		return err
	}
	if record.State == "pending" || !bytes.Equal(reportTerminalIndexKey(record), indexKey) {
		return errors.New("report delivery ledger retention index is inconsistent")
	}
	if err := records.Delete([]byte(deliveryID)); err != nil {
		return err
	}
	return terminal.Delete(indexKey)
}

func hashString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
