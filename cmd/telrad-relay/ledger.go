package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
)

const ledgerFileName = "accessions.ledger"

// ledger is the set of accessions this Relay has seen Telrad accept. It is an
// append-only file with one accession per line, and the only clinical state
// Relay keeps.
type ledger struct {
	mu      sync.Mutex
	file    *os.File
	size    int64
	entries map[string]struct{}
}

// openLedger loads every complete line. A final line without its newline is
// the remains of an append that never synced, so its acknowledgement was
// never forwarded; it is truncated rather than trusted or extended.
//
// The file is deliberately not opened with O_APPEND: on Windows such a handle
// lacks write-data access and cannot be truncated. Appends instead write at the
// tracked end offset under the mutex, and this process is the only writer.
func openLedger(path string) (*ledger, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]struct{})
	reader := bufio.NewReaderSize(file, 64*1024)
	var size int64
	for {
		line, readErr := reader.ReadString('\n')
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			file.Close()
			return nil, readErr
		}
		size += int64(len(line))
		if value := strings.TrimRight(line, "\r\n"); value != "" {
			entries[value] = struct{}{}
		}
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.Size() != size {
		if err := file.Truncate(size); err != nil {
			file.Close()
			return nil, err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return nil, err
		}
	}
	return &ledger{file: file, size: size, entries: entries}, nil
}

func (store *ledger) contains(accession string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	_, ok := store.entries[accession]
	return ok
}

// append records accessions durably. It returns only after the bytes are
// synced, so the caller may forward the acknowledgement that produced them.
func (store *ledger) append(accessions []string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	var pending []string
	seen := make(map[string]struct{}, len(accessions))
	for _, accession := range accessions {
		if accession == "" || strings.ContainsAny(accession, "\r\n") {
			return errors.New("accession cannot be recorded")
		}
		if _, exists := store.entries[accession]; exists {
			continue
		}
		if _, duplicate := seen[accession]; duplicate {
			continue
		}
		seen[accession] = struct{}{}
		pending = append(pending, accession)
	}
	if len(pending) == 0 {
		return nil
	}
	var builder strings.Builder
	for _, accession := range pending {
		builder.WriteString(accession)
		builder.WriteByte('\n')
	}
	written, err := store.file.WriteAt([]byte(builder.String()), store.size)
	if err == nil {
		err = store.file.Sync()
	}
	if err != nil {
		// Never leave a partial line for the next append to extend.
		_ = store.file.Truncate(store.size)
		return err
	}
	store.size += int64(written)
	for _, accession := range pending {
		store.entries[accession] = struct{}{}
	}
	return nil
}

func (store *ledger) count() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.entries)
}

func (store *ledger) close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.file.Close()
}
