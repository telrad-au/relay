package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestReportLedgerDurablyDeduplicatesAcceptedDelivery(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	ledger := openReportLedgerForTest(t, configPath)
	now := time.Now().UTC()
	if _, fresh, err := ledger.Begin("delivery-1", "secret-token", "payload-hash", "control-1", now); err != nil || !fresh {
		t.Fatalf("begin delivery: fresh=%v error=%v", fresh, err)
	}
	if err := ledger.Complete("delivery-1", "secret-token", "accepted", "AA", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	closeReportLedgerForTest(t, ledger)

	reloaded := openReportLedgerForTest(t, configPath)
	record, fresh, err := reloaded.Begin("delivery-1", "secret-token", "payload-hash", "control-1", now.Add(2*time.Second))
	if err != nil || fresh || record.State != "accepted" || record.AckCode != "AA" {
		t.Fatalf("durable duplicate lookup = %#v, fresh=%v, error=%v", record, fresh, err)
	}
	closeReportLedgerForTest(t, reloaded)

	contents, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), reportLedgerFilename))
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) == 0 || json.Valid(contents) {
		t.Fatal("ledger is not a non-JSON transactional store")
	}
	for _, plaintext := range []string{"secret-token", "control-1"} {
		if bytes.Contains(contents, []byte(plaintext)) {
			t.Fatalf("ledger contains plaintext %q", plaintext)
		}
	}
}

func TestReportLedgerPreservesAmbiguousCrashWindowAndRejectsReuse(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	ledger := openReportLedgerForTest(t, configPath)
	now := time.Now().UTC()
	if _, fresh, err := ledger.Begin("delivery-2", "token", "payload", "control", now); err != nil || !fresh {
		t.Fatalf("begin delivery: fresh=%v error=%v", fresh, err)
	}
	closeReportLedgerForTest(t, ledger)

	reloaded := openReportLedgerForTest(t, configPath)
	record, fresh, err := reloaded.Begin("delivery-2", "token", "payload", "control", now.Add(time.Second))
	if err != nil || fresh || record.State != "pending" {
		t.Fatalf("crash-window lookup = %#v, fresh=%v, error=%v", record, fresh, err)
	}
	for name, values := range map[string][3]string{
		"token":   {"different-token", "payload", "control"},
		"payload": {"token", "different-payload", "control"},
		"control": {"token", "payload", "different-control"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := reloaded.Begin("delivery-2", values[0], values[1], values[2], now); err == nil {
				t.Fatal("delivery identifier reuse with different metadata was accepted")
			}
		})
	}
	closeReportLedgerForTest(t, reloaded)
}

func TestReportLedgerDurablyPreservesRejectedDeliveryForReconciliation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	ledger := openReportLedgerForTest(t, configPath)
	now := time.Now().UTC()
	if _, fresh, err := ledger.Begin("delivery-rejected", "token", "payload", "control", now); err != nil || !fresh {
		t.Fatalf("begin delivery: fresh=%v error=%v", fresh, err)
	}
	if err := ledger.Complete("delivery-rejected", "token", "rejected", "AR", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	closeReportLedgerForTest(t, ledger)

	reloaded := openReportLedgerForTest(t, configPath)
	record, fresh, err := reloaded.Begin("delivery-rejected", "token", "payload", "control", now.Add(2*time.Second))
	if err != nil || fresh || record.State != "rejected" || record.AckCode != "AR" {
		t.Fatalf("rejected duplicate = %#v, fresh=%v, error=%v", record, fresh, err)
	}
	closeReportLedgerForTest(t, reloaded)
}

func TestReportLedgerSurvivesAbruptProcessExit(t *testing.T) {
	const (
		stateVariable = "TELRAD_TEST_REPORT_LEDGER_CRASH_STATE"
		pathVariable  = "TELRAD_TEST_REPORT_LEDGER_CRASH_CONFIG"
	)
	if state := os.Getenv(stateVariable); state != "" {
		ledger, err := openReportDeliveryLedger(os.Getenv(pathVariable))
		if err != nil {
			os.Exit(2)
		}
		now := time.Unix(1700000000, 0).UTC()
		if _, fresh, err := ledger.Begin("crash-delivery", "crash-token", "crash-payload", "crash-control", now); err != nil || !fresh {
			os.Exit(3)
		}
		if state == "accepted" {
			if err := ledger.Complete("crash-delivery", "crash-token", "accepted", "AA", now.Add(time.Second)); err != nil {
				os.Exit(4)
			}
		}
		os.Exit(0)
	}

	for _, state := range []string{"pending", "accepted"} {
		t.Run(state, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			command := exec.Command(os.Args[0], "-test.run=^TestReportLedgerSurvivesAbruptProcessExit$")
			command.Env = append(os.Environ(), stateVariable+"="+state, pathVariable+"="+configPath)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("crash helper: %v\n%s", err, output)
			}
			ledger := openReportLedgerForTest(t, configPath)
			record, fresh, err := ledger.Begin("crash-delivery", "crash-token", "crash-payload", "crash-control", time.Now().UTC())
			if err != nil || fresh || record.State != state {
				t.Fatalf("record after abrupt exit = %#v, fresh=%v, error=%v", record, fresh, err)
			}
			closeReportLedgerForTest(t, ledger)
		})
	}
}

func TestReportLedgerTransactionsRollBackOnStorageFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	ledger := openReportLedgerForTest(t, configPath)
	defer closeReportLedgerForTest(t, ledger)
	now := time.Now().UTC()
	injected := errors.New("injected storage failure")

	ledger.update = rollbackReportLedgerUpdate(ledger.db, injected)
	if _, _, err := ledger.Begin("begin-failure", "token", "payload", "control", now); !errors.Is(err, injected) {
		t.Fatalf("begin error = %v", err)
	}
	if _, found, err := reportLedgerRecord(ledger.db, "begin-failure"); err != nil || found {
		t.Fatalf("rolled-back begin found=%v error=%v", found, err)
	}

	ledger.update = ledger.db.Update
	if _, fresh, err := ledger.Begin("complete-failure", "token", "payload", "control", now); err != nil || !fresh {
		t.Fatalf("begin delivery: fresh=%v error=%v", fresh, err)
	}
	ledger.update = rollbackReportLedgerUpdate(ledger.db, injected)
	if err := ledger.Complete("complete-failure", "token", "accepted", "AA", now.Add(time.Second)); !errors.Is(err, injected) {
		t.Fatalf("complete error = %v", err)
	}
	ledger.update = ledger.db.Update
	record, fresh, err := ledger.Begin("complete-failure", "token", "payload", "control", now.Add(2*time.Second))
	if err != nil || fresh || record.State != "pending" {
		t.Fatalf("rolled-back completion = %#v, fresh=%v, error=%v", record, fresh, err)
	}
}

func TestReportLedgerMigratesLegacyJSONWithRecoverableHashOnlyBackup(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	ledgerPath := filepath.Join(directory, reportLedgerFilename)
	legacyPath := filepath.Join(directory, reportLedgerLegacyFilename)
	now := time.Now().UTC()
	legacy := map[string]reportDeliveryRecord{
		"legacy-accepted": {
			DeliveryID: "legacy-accepted", TokenSHA256: hashString("secret-token"), PayloadSHA256: "payload-hash",
			LegacyControl: "plaintext-control", State: "accepted", AckCode: "AA", UpdatedAt: now,
		},
		"legacy-pending": {
			DeliveryID: "legacy-pending", TokenSHA256: hashString("pending-token"), PayloadSHA256: "pending-payload",
			ControlIDHash: hashString("pending-control"), State: "pending", UpdatedAt: now.Add(time.Second),
		},
	}
	if err := atomicWriteJSON(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}

	ledger := openReportLedgerForTest(t, configPath)
	closeReportLedgerForTest(t, ledger)
	store, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(directory, reportLedgerBackupFilename)
	normalizedLegacy, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string][]byte{"store": store, "legacy": normalizedLegacy, "backup": backup} {
		for _, plaintext := range []string{"secret-token", "plaintext-control", "pending-token", "pending-control"} {
			if bytes.Contains(contents, []byte(plaintext)) {
				t.Fatalf("%s contains plaintext %q", name, plaintext)
			}
		}
	}
	if json.Valid(store) || !json.Valid(normalizedLegacy) || !json.Valid(backup) {
		t.Fatal("migration did not create the database and normalized JSON recovery files")
	}
	var normalized map[string]reportDeliveryRecord
	if err := json.Unmarshal(backup, &normalized); err != nil {
		t.Fatal(err)
	}
	if record := normalized["legacy-accepted"]; record.LegacyControl != "" || record.ControlIDHash != hashString("plaintext-control") {
		t.Fatalf("normalized legacy record = %#v", record)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{ledgerPath, legacyPath, backupPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("%s mode = %o", filepath.Base(path), info.Mode().Perm())
			}
		}
	}

	reloaded := openReportLedgerForTest(t, configPath)
	record, fresh, err := reloaded.Begin("legacy-accepted", "secret-token", "payload-hash", "plaintext-control", now.Add(2*time.Second))
	if err != nil || fresh || record.State != "accepted" {
		t.Fatalf("migrated duplicate = %#v, fresh=%v, error=%v", record, fresh, err)
	}
	if _, fresh, err := reloaded.Begin("post-migration", "new-token", "new-payload", "new-control", now.Add(3*time.Second)); err != nil || !fresh {
		t.Fatalf("post-migration begin: fresh=%v error=%v", fresh, err)
	}
	closeReportLedgerForTest(t, reloaded)
	marker, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != reportLedgerLegacyMarker {
		t.Fatalf("legacy ledger marker = %q", marker)
	}
}

func TestReportLedgerDoesNotFallBackToMigrationBackup(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	ledgerPath := filepath.Join(directory, reportLedgerFilename)
	legacyPath := filepath.Join(directory, reportLedgerLegacyFilename)
	legacy := map[string]reportDeliveryRecord{
		"delivery": testReportRecord("delivery", "accepted", time.Now().UTC()),
	}
	if err := atomicWriteJSON(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}
	ledger := openReportLedgerForTest(t, configPath)
	closeReportLedgerForTest(t, ledger)
	if err := os.WriteFile(ledgerPath, []byte("corrupt database"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openReportDeliveryLedger(configPath); err == nil {
		t.Fatal("corrupt transactional ledger fell back to stale migration backup")
	}
}

func TestReportLedgerDatabaseRemainsAuthoritativeOverLegacySnapshot(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	legacyPath := filepath.Join(directory, reportLedgerLegacyFilename)
	now := time.Now().UTC()
	legacy := map[string]reportDeliveryRecord{
		"delivery": testReportRecord("delivery", "accepted", now),
	}
	if err := atomicWriteJSON(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}
	ledger := openReportLedgerForTest(t, configPath)
	closeReportLedgerForTest(t, ledger)
	if err := atomicWriteJSON(legacyPath, map[string]reportDeliveryRecord{}); err != nil {
		t.Fatal(err)
	}

	reloaded := openReportLedgerForTest(t, configPath)
	record, found, err := reportLedgerRecord(reloaded.db, "delivery")
	if err != nil || !found || record.State != "accepted" {
		t.Fatalf("authoritative record = %#v, found=%v, error=%v", record, found, err)
	}
	closeReportLedgerForTest(t, reloaded)
}

func TestReportLedgerFailsClosedWhenMigrationMarkerOutlivesDatabase(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, reportLedgerLegacyFilename), []byte(reportLedgerLegacyMarker), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openReportDeliveryLedger(filepath.Join(directory, "relay.json")); err == nil || !strings.Contains(err.Error(), "transactional ledger is missing") {
		t.Fatalf("missing transactional ledger error = %v", err)
	}
}

func TestReportLedgerPrunesExpiredTerminalRecordsButRetainsPending(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	now := time.Now().UTC()
	initial := map[string]reportDeliveryRecord{
		"expired": testReportRecord("expired", "accepted", now.Add(-reportLedgerRetention-time.Second)),
		"recent":  testReportRecord("recent", "rejected", now.Add(-time.Hour)),
		"pending": testReportRecord("pending", "pending", now.Add(-2*reportLedgerRetention)),
	}
	if err := atomicWriteJSON(filepath.Join(directory, reportLedgerLegacyFilename), initial); err != nil {
		t.Fatal(err)
	}
	ledger := openReportLedgerForTest(t, configPath)
	defer closeReportLedgerForTest(t, ledger)
	if _, fresh, err := ledger.Begin("new", "token-new", "payload-new", "control-new", now); err != nil || !fresh {
		t.Fatalf("begin after pruning: fresh=%v error=%v", fresh, err)
	}
	for deliveryID, want := range map[string]bool{"expired": false, "recent": true, "pending": true, "new": true} {
		if _, found, err := reportLedgerRecord(ledger.db, deliveryID); err != nil || found != want {
			t.Fatalf("record %q found=%v, want %v, error=%v", deliveryID, found, want, err)
		}
	}
	if count := reportLedgerCountForTest(t, ledger); count != 3 {
		t.Fatalf("record count = %d, want 3", count)
	}
}

func TestReportLedgerBoundsExpiredRecordPruningPerTransition(t *testing.T) {
	directory := t.TempDir()
	now := time.Now().UTC()
	initial := make(map[string]reportDeliveryRecord, reportLedgerPruneBatchSize+8)
	for index := 0; index < reportLedgerPruneBatchSize+8; index++ {
		deliveryID := fmt.Sprintf("expired-%02d", index)
		initial[deliveryID] = testReportRecord(deliveryID, "accepted", now.Add(-reportLedgerRetention-time.Duration(index+1)*time.Second))
	}
	if err := atomicWriteJSON(filepath.Join(directory, reportLedgerLegacyFilename), initial); err != nil {
		t.Fatal(err)
	}
	ledger := openReportLedgerForTest(t, filepath.Join(directory, "relay.json"))
	defer closeReportLedgerForTest(t, ledger)
	if _, fresh, err := ledger.Begin("new", "token", "payload", "control", now); err != nil || !fresh {
		t.Fatalf("begin after pruning: fresh=%v error=%v", fresh, err)
	}
	want := uint64(len(initial) - reportLedgerPruneBatchSize + 1)
	if count := reportLedgerCountForTest(t, ledger); count != want {
		t.Fatalf("record count = %d, want %d", count, want)
	}
}

func TestReportLedgerEvictsOldestTerminalRecordAtCapacity(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	now := time.Now().UTC()
	initial := make(map[string]reportDeliveryRecord, reportLedgerMaxEntries)
	initial["oldest-terminal"] = testReportRecord("oldest-terminal", "accepted", now.Add(-time.Hour))
	for index := 1; index < reportLedgerMaxEntries; index++ {
		deliveryID := fmt.Sprintf("pending-%05d", index)
		initial[deliveryID] = testReportRecord(deliveryID, "pending", now)
	}
	if err := atomicWriteJSON(filepath.Join(directory, reportLedgerLegacyFilename), initial); err != nil {
		t.Fatal(err)
	}
	ledger := openReportLedgerForTest(t, configPath)
	defer closeReportLedgerForTest(t, ledger)
	if _, fresh, err := ledger.Begin("replacement", "token-replacement", "payload-replacement", "control-replacement", now); err != nil || !fresh {
		t.Fatalf("begin at capacity: fresh=%v error=%v", fresh, err)
	}
	if _, found, err := reportLedgerRecord(ledger.db, "oldest-terminal"); err != nil || found {
		t.Fatalf("oldest terminal record found=%v error=%v", found, err)
	}
	if count := reportLedgerCountForTest(t, ledger); count != reportLedgerMaxEntries {
		t.Fatalf("record count = %d, want %d", count, reportLedgerMaxEntries)
	}
	if _, _, err := ledger.Begin("over-capacity", "token", "payload", "control", now); err == nil || !strings.Contains(err.Error(), "ledger is full") {
		t.Fatalf("all-pending capacity error = %v", err)
	}
}

func BenchmarkReportLedgerTransitions(b *testing.B) {
	for _, recordCount := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("begin/%d_records", recordCount), func(b *testing.B) {
			benchmarkReportLedgerBegin(b, recordCount)
		})
		b.Run(fmt.Sprintf("complete/%d_records", recordCount), func(b *testing.B) {
			benchmarkReportLedgerComplete(b, recordCount)
		})
	}
}

func benchmarkReportLedgerBegin(b *testing.B, recordCount int) {
	ledger, now := benchmarkReportLedger(b, recordCount-1, "accepted")
	defer ledger.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	var pageBytes, writes int64
	for index := 0; index < b.N; index++ {
		deliveryID := fmt.Sprintf("benchmark-new-%d", index)
		before := ledger.db.Stats()
		b.StartTimer()
		_, fresh, err := ledger.Begin(deliveryID, "benchmark-token", "benchmark-payload", "benchmark-control", now)
		b.StopTimer()
		if err != nil || !fresh {
			b.Fatalf("begin: fresh=%v error=%v", fresh, err)
		}
		after := ledger.db.Stats()
		delta := after.Sub(&before)
		pageBytes += delta.TxStats.GetPageAlloc()
		writes += delta.TxStats.GetWrite()
		deleteReportRecordForBenchmark(b, ledger, deliveryID)
	}
	reportLedgerBenchmarkMetrics(b, pageBytes, writes)
}

func benchmarkReportLedgerComplete(b *testing.B, recordCount int) {
	ledger, now := benchmarkReportLedger(b, recordCount, "pending")
	defer ledger.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	var pageBytes, writes int64
	for index := 0; index < b.N; index++ {
		deliveryID := fmt.Sprintf("benchmark-%05d", index%recordCount)
		before := ledger.db.Stats()
		b.StartTimer()
		err := ledger.Complete(deliveryID, "token-"+deliveryID, "accepted", "AA", now.Add(time.Second))
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		after := ledger.db.Stats()
		delta := after.Sub(&before)
		pageBytes += delta.TxStats.GetPageAlloc()
		writes += delta.TxStats.GetWrite()
		resetReportRecordForBenchmark(b, ledger, deliveryID, now)
	}
	reportLedgerBenchmarkMetrics(b, pageBytes, writes)
}

func benchmarkReportLedger(b *testing.B, recordCount int, state string) (*reportDeliveryLedger, time.Time) {
	b.Helper()
	directory := b.TempDir()
	now := time.Now().UTC()
	initial := make(map[string]reportDeliveryRecord, recordCount)
	for index := 0; index < recordCount; index++ {
		deliveryID := fmt.Sprintf("benchmark-%05d", index)
		initial[deliveryID] = testReportRecord(deliveryID, state, now)
	}
	if err := createReportLedgerDatabase(filepath.Join(directory, reportLedgerFilename), initial); err != nil {
		b.Fatal(err)
	}
	ledger, err := openReportDeliveryLedger(filepath.Join(directory, "relay.json"))
	if err != nil {
		b.Fatal(err)
	}
	if err := ledger.invalidateLegacyLedger(); err != nil {
		b.Fatal(err)
	}
	return ledger, now
}

func deleteReportRecordForBenchmark(b *testing.B, ledger *reportDeliveryLedger, deliveryID string) {
	b.Helper()
	if err := ledger.db.Update(func(tx *bolt.Tx) error {
		records := tx.Bucket(reportLedgerRecordsBucket)
		metadata := tx.Bucket(reportLedgerMetadataBucket)
		if err := records.Delete([]byte(deliveryID)); err != nil {
			return err
		}
		count, err := reportLedgerCount(metadata)
		if err != nil {
			return err
		}
		return putReportLedgerCount(metadata, count-1)
	}); err != nil {
		b.Fatal(err)
	}
}

func resetReportRecordForBenchmark(b *testing.B, ledger *reportDeliveryLedger, deliveryID string, now time.Time) {
	b.Helper()
	if err := ledger.db.Update(func(tx *bolt.Tx) error {
		records := tx.Bucket(reportLedgerRecordsBucket)
		terminal := tx.Bucket(reportLedgerTerminalBucket)
		data := records.Get([]byte(deliveryID))
		record, err := decodeStoredReportRecord(deliveryID, data)
		if err != nil {
			return err
		}
		if err := terminal.Delete(reportTerminalIndexKey(record)); err != nil {
			return err
		}
		record.State = "pending"
		record.AckCode = ""
		record.UpdatedAt = now
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return records.Put([]byte(deliveryID), encoded)
	}); err != nil {
		b.Fatal(err)
	}
}

func reportLedgerBenchmarkMetrics(b *testing.B, pageBytes, writes int64) {
	b.Helper()
	if b.N == 0 {
		return
	}
	b.ReportMetric(float64(pageBytes)/float64(b.N), "page-bytes/transition")
	b.ReportMetric(float64(writes)/float64(b.N), "writes/transition")
}

func rollbackReportLedgerUpdate(db *bolt.DB, injected error) func(func(*bolt.Tx) error) error {
	return func(update func(*bolt.Tx) error) error {
		tx, err := db.Begin(true)
		if err != nil {
			return err
		}
		if err := update(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Rollback(); err != nil {
			return err
		}
		return injected
	}
}

func testReportRecord(deliveryID, state string, updatedAt time.Time) reportDeliveryRecord {
	return reportDeliveryRecord{
		DeliveryID: deliveryID, TokenSHA256: hashString("token-" + deliveryID), PayloadSHA256: "payload-" + deliveryID,
		ControlIDHash: hashString("control-" + deliveryID), State: state, AckCode: reportAckCode(state), UpdatedAt: updatedAt,
	}
}

func reportAckCode(state string) string {
	if state == "accepted" {
		return "AA"
	}
	if state == "rejected" {
		return "AE"
	}
	return ""
}

func openReportLedgerForTest(t *testing.T, configPath string) *reportDeliveryLedger {
	t.Helper()
	ledger, err := openReportDeliveryLedger(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func closeReportLedgerForTest(t *testing.T, ledger *reportDeliveryLedger) {
	t.Helper()
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}

func reportLedgerCountForTest(t *testing.T, ledger *reportDeliveryLedger) uint64 {
	t.Helper()
	var count uint64
	if err := ledger.db.View(func(tx *bolt.Tx) error {
		var err error
		count, err = reportLedgerCount(tx.Bucket(reportLedgerMetadataBucket))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return count
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
