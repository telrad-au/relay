package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLedgerAppendReloadAndDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ledgerFileName)
	store, err := openLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.append([]string{"ACC1", "ACC2", "ACC1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.append([]string{"ACC2"}); err != nil {
		t.Fatal(err)
	}
	if err := store.append(nil); err != nil {
		t.Fatal(err)
	}
	if !store.contains("ACC1") || !store.contains("ACC2") || store.contains("ACC3") || store.count() != 2 {
		t.Fatal("ledger contents wrong")
	}
	store.close()
	data, _ := os.ReadFile(path)
	if string(data) != "ACC1\nACC2\n" {
		t.Fatalf("file=%q", data)
	}
	assertPrivateFileMode(t, path)
	reloaded, err := openLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.close()
	if !reloaded.contains("ACC1") || reloaded.count() != 2 {
		t.Fatal("reload lost entries")
	}
	if err := reloaded.append([]string{"bad\nvalue"}); err == nil {
		t.Fatal("line break accepted")
	}
	if err := reloaded.append([]string{""}); err == nil {
		t.Fatal("empty accession accepted")
	}
}

// A line cut short by a crash was never synced, so its acknowledgement was
// never forwarded. It must not authorise anything or absorb the next entry.
func TestLedgerTruncatesUnsyncedPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ledgerFileName)
	if err := os.WriteFile(path, []byte("ACC1\nACC2\nACC12345"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if !store.contains("ACC1") || !store.contains("ACC2") || store.contains("ACC12345") || store.count() != 2 {
		t.Fatal("partial line trusted")
	}
	if data, _ := os.ReadFile(path); string(data) != "ACC1\nACC2\n" {
		t.Fatalf("partial line kept: file=%q", data)
	}
	if err := store.append([]string{"ACC3"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "ACC1\nACC2\nACC3\n" {
		t.Fatalf("file=%q", data)
	}
}
