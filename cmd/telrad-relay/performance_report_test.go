package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

// All clocks, polling delays, result retry backoffs and RIS deadlines are the
// real implementation. Run fault cases explicitly with -benchtime=1x.
func BenchmarkReportReturn(b *testing.B) {
	for _, scenario := range []string{"backlog", "idle", "delayed", "negative", "missing", "malformed", "lost-result", "expired-claim"} {
		b.Run(scenario, func(b *testing.B) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var sends, polls, results, committed atomic.Int64
			go func() {
				for {
					c, err := listener.Accept()
					if err != nil {
						return
					}
					go func() {
						defer c.Close()
						sends.Add(1)
						stop := context.AfterFunc(ctx, func() { _ = c.Close() })
						defer stop()
						frame, err := readMLLPFrame(c, 1024*1024)
						if err != nil {
							return
						}
						id, err := hl7ControlID(frame[1 : len(frame)-2])
						if err != nil {
							return
						}
						if scenario == "missing" {
							<-ctx.Done()
							return
						}
						if scenario == "delayed" {
							time.Sleep(50 * time.Millisecond)
						}
						code := "AA"
						if scenario == "negative" {
							code = "AE"
						}
						ack := synthetic.ACK(code, id, "benchmark-ack")
						if scenario == "malformed" {
							ack = []byte("invalid ACK")
						}
						_, _ = c.Write(synthetic.Frame(ack))
					}()
				}
			}()
			cfg := defaultConfig()
			cfg.ReportHost = "127.0.0.1"
			cfg.ReportPort = listener.Addr().(*net.TCPAddr).Port
			credentialPath := filepath.Join(b.TempDir(), "credential.json")
			if err := commitCredential(credentialPath, credentialFile{SchemaVersion: 1, Credential: testCredential('P')}); err != nil {
				b.Fatal(err)
			}
			provider, err := newCredentialProvider(credentialPath, time.Now())
			if err != nil {
				b.Fatal(err)
			}
			// Precompute synthetic payloads and digests before timed exchanges.
			payloads := make([][]byte, b.N)
			digests := make([]string, b.N)
			for i := range payloads {
				payloads[i] = synthetic.HL7(fmt.Sprintf("benchmark-%d", i), 4096)
				sum := sha256.Sum256(payloads[i])
				digests[i] = hex.EncodeToString(sum[:])
			}
			var attempt int
			var deadline time.Time
			var lastResult reportResult
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodDelete:
					w.WriteHeader(204)
				case strings.HasSuffix(r.URL.Path, "/sessions"):
					w.WriteHeader(201)
					_ = json.NewEncoder(w).Encode(readyMessage{Type: "ready", SessionID: "benchmark-session", Transports: map[string]readyTransport{"dicom": {URL: cfg.DicomURL, ContentType: "application/dicom"}, "hl7": {URL: cfg.HL7URL, ContentType: "application/hl7-v2"}}})
				case strings.HasSuffix(r.URL.Path, "/poll"):
					polls.Add(1)
					if committed.Load() == int64(b.N) {
						// The next poll proves Relay finished the previous result
						// exchange, including receipt of the cloud confirmation.
						cancel()
						w.WriteHeader(204)
						return
					}
					if scenario == "idle" && polls.Load() == 1 {
						w.WriteHeader(204)
						return
					}
					attempt++
					deadline = time.Now().Add(time.Minute)
					id := fmt.Sprintf("benchmark-%d", committed.Load())
					payload := payloads[committed.Load()]
					_ = json.NewEncoder(w).Encode(reportMessage{Type: "report", DeliveryID: id, Token: fmt.Sprintf("token-%d", attempt), MessageControlID: id, Payload: string(payload), PayloadSHA256: digests[committed.Load()], ClaimExpiresAt: deadline})
				case strings.HasSuffix(r.URL.Path, "/result"):
					results.Add(1)
					var result reportResult
					if json.NewDecoder(r.Body).Decode(&result) != nil {
						b.Error("invalid result")
						cancel()
						return
					}
					if scenario == "expired-claim" && attempt == 1 {
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(503)
						return
					}
					if scenario == "lost-result" && results.Load()%2 == 1 {
						lastResult = result
						panic(http.ErrAbortHandler)
					}
					if scenario == "lost-result" && result != lastResult {
						b.Error("result replay changed")
					}
					_, _ = w.Write([]byte(`{"ok":true}`))
					committed.Add(1)
				}
			}))
			defer cloud.Close()
			cfg.ControlURL = cloud.URL
			status := newRuntimeStatus(filepath.Join(b.TempDir(), "relay.json"))
			b.ReportAllocs()
			b.SetBytes(4096)
			b.ResetTimer()
			superviseControl(ctx, ctx, cfg, cloud.Client(), provider, make(chan struct{}), newWorkDrainer(), status)
			b.StopTimer()
			if committed.Load() != int64(b.N) {
				b.Fatal("report workflow did not complete")
			}
			b.ReportMetric(float64(sends.Load())/float64(b.N), "ris-sends/op")
			b.ReportMetric(float64(results.Load())/float64(b.N), "result-requests/op")
			b.ReportMetric(float64(polls.Load())/float64(b.N), "polls/op")
		})
	}
}

// TestPerformanceDiagnostic runs the real runtime with test-only observation.
// Supply a synthetic configuration and drive it from the external harness. Go's
// -cpuprofile/-memprofile flags collect profiles without a production endpoint.
func TestPerformanceDiagnostic(t *testing.T) {
	path := os.Getenv("TELRAD_PERF_CONFIG")
	if path == "" {
		t.Skip("set TELRAD_PERF_CONFIG for an explicit synthetic diagnostic run")
	}
	duration, err := time.ParseDuration(os.Getenv("TELRAD_PERF_DURATION"))
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		t.Fatal("set a bounded TELRAD_PERF_DURATION")
	}
	output := os.Getenv("TELRAD_PERF_METRICS")
	if output == "" {
		t.Fatal("set TELRAD_PERF_METRICS")
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cfg, err := loadConfigMode(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RelayID != "perf-relay" {
		t.Fatal("diagnostics require a harness-generated synthetic configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runClinicalWithContext(ctx, cfg, path) }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	encoder := json.NewEncoder(file)
	record := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		_ = encoder.Encode(map[string]any{"at": time.Now().UTC(), "goroutines": runtime.NumGoroutine(), "heapAlloc": m.HeapAlloc, "heapInuse": m.HeapInuse, "heapSys": m.HeapSys, "totalAlloc": m.TotalAlloc, "mallocs": m.Mallocs, "gcCycles": m.NumGC})
	}
	record()
	for {
		select {
		case <-ticker.C:
			record()
		case err := <-done:
			record()
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-ctx.Done():
			select {
			case err := <-done:
				record()
				if err != nil {
					t.Fatal(err)
				}
				return
			case <-time.After(75 * time.Second):
				t.Fatal("diagnostic runtime did not drain")
			}
		}
	}
}
