package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPollingRetriesLostResultWithoutRedeliveringAndDrains(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			frame, err := readMLLPFrame(conn, 1024*1024)
			if err == nil {
				received <- string(frame)
				_, _ = conn.Write([]byte("\x0bMSH|^~\\&|RIS|TEST|TELRAD|TEST|20260906000000||ACK|ack-1|P|2.5\rMSA|AA|report-1\r\x1c\r"))
			}
			_ = conn.Close()
		}
	}()
	payload := "MSH|^~\\&|TELRAD|TEST|RIS|TEST|20260906000000||ORU^R01|report-1|P|2.5\r"
	digest := sha256.Sum256([]byte(payload))
	cfg := pairedTestConfig(t.TempDir())
	cfg.ReportHost = "127.0.0.1"
	cfg.ReportPort = listener.Addr().(*net.TCPAddr).Port
	provider := testProvider(t, testCredential('A'))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workCtx, stopWork := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopWork()
	changed := make(chan struct{}, 1)
	var mu sync.Mutex
	var results []reportResult
	polls := 0
	closed := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testCredential('A') || r.Header.Get("X-Telrad-Protocol-Version") != "1" {
			t.Error("missing control authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			close(closed)
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/sessions"):
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(readyMessage{Type: "ready", SessionID: "session-1", ConnectorID: cfg.RelayID, IngestMode: "PRODUCTION", Transports: map[string]readyTransport{"dicom": {URL: cfg.DicomURL, ContentType: "application/dicom"}, "hl7": {URL: cfg.HL7URL, ContentType: "application/hl7-v2"}}})
		case strings.HasSuffix(r.URL.Path, "/poll"):
			mu.Lock()
			polls++
			attempt := polls
			mu.Unlock()
			// A later cloud claim is deliberately sent again with identical bytes.
			_ = json.NewEncoder(w).Encode(reportMessage{Type: "report", DeliveryID: "delivery-1", Token: []string{"token-1", "token-2"}[attempt-1], MessageControlID: "report-1", Payload: payload, PayloadSHA256: hex.EncodeToString(digest[:]), ClaimExpiresAt: time.Now().Add(8 * time.Second)})
		case strings.HasSuffix(r.URL.Path, "/result"):
			var result reportResult
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				t.Error(err)
			}
			mu.Lock()
			results = append(results, result)
			attempt := len(results)
			mu.Unlock()
			if attempt == 1 {
				// Simulate the server committing the result but losing its response.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				changed <- struct{}{}
				return
			}
			if attempt == 3 {
				cancel()
			} // Stop admission while this result is in flight.
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg.ControlURL = server.URL + "/v1/relay/control"
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseControl(ctx, workCtx, cfg, server.Client(), provider, changed, newWorkDrainer(), newRuntimeStatus(cfg.configPath))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("polling did not drain")
	}
	select {
	case <-closed:
	default:
		t.Fatal("session not closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 3 || !reflect.DeepEqual(results[0], results[1]) || results[2].Token != "token-2" {
		t.Fatalf("unexpected result replay: %+v", results)
	}
	for _, result := range results {
		if result.Outcome != "accepted" || result.AckCode != "AA" {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if len(received) != 2 {
		t.Fatalf("got %d MLLP deliveries, want 2", len(received))
	}
	if first, second := <-received, <-received; first != second {
		t.Fatal("cloud retry changed MLLP bytes")
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.configPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "ledger") {
			t.Fatal("report ledger created")
		}
	}
}

func TestPollingSchemaUpgradePreservesCredentialsAndRejectsForeignOrigin(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserves", true: "rejects"}[foreign], func(t *testing.T) {
			directory := t.TempDir()
			cfg := pairedTestConfig(directory)
			cfg.SchemaVersion = 3
			cfg.ControlURL = strings.Replace(cfg.ControlURL, "https:", "wss:", 1)
			if foreign {
				cfg.ControlURL = "wss://foreign.example/v1/relay/control"
			}
			if err := atomicWriteJSON(cfg.configPath, cfg); err != nil {
				t.Fatal(err)
			}
			credential := []byte("synthetic credential remains byte-identical")
			if err := os.WriteFile(cfg.CredentialPath, credential, 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(cfg.configPath)
			err := upgradePollingConfig(cfg.configPath, cfg)
			if foreign {
				if err == nil {
					t.Fatal("foreign origin accepted")
				}
				after, _ := os.ReadFile(cfg.configPath)
				if string(before) != string(after) {
					t.Fatal("rejected upgrade changed config")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SchemaVersion != 4 || !strings.HasPrefix(cfg.ControlURL, "https:") {
				t.Fatal("configuration not upgraded")
			}
			after, err := os.ReadFile(cfg.CredentialPath)
			if err != nil || string(after) != string(credential) {
				t.Fatal("credential changed")
			}
			if err := upgradePollingConfig(cfg.configPath, cfg); err != nil {
				t.Fatal("upgrade not idempotent", err)
			}
		})
	}
}
