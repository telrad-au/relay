package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

func TestReportDestinationPolicy(t *testing.T) {
	tests := []struct {
		name    string
		pin     *reportDestination
		ready   string // "" means an older server that never sends the field
		allowed []string
		want    *reportDestination
	}{
		{name: "older server without pin", ready: ""},
		{name: "older server keeps pin", pin: &reportDestination{"127.0.0.1", 2576}, want: &reportDestination{"127.0.0.1", 2576}},
		{name: "no receiver in Telrad", ready: "null"},
		{name: "no receiver in Telrad keeps pin", pin: &reportDestination{"127.0.0.1", 2576}, ready: "null", want: &reportDestination{"127.0.0.1", 2576}},
		{name: "Telrad private destination", ready: `{"host":"192.168.61.20","port":2576}`, want: &reportDestination{"192.168.61.20", 2576}},
		{name: "Telrad public destination rejected", ready: `{"host":"8.8.8.8","port":2576}`},
		{name: "non-canonical host rejected", ready: `{"host":"192.168.061.20","port":2576}`},
		{name: "host name rejected", ready: `{"host":"ris.clinic.example","port":2576}`},
		{name: "invalid port rejected", ready: `{"host":"192.168.61.20","port":0}`},
		{name: "unknown field rejected", ready: `{"host":"192.168.61.20","port":2576,"via":"cloud"}`},
		{name: "allowlist override", ready: `{"host":"203.0.113.9","port":2576}`, allowed: []string{"203.0.113.0/24"}, want: &reportDestination{"203.0.113.9", 2576}},
		{name: "allowlist override excludes defaults", ready: `{"host":"192.168.61.20","port":2576}`, allowed: []string{"203.0.113.0/24"}},
		{name: "matching pin", pin: &reportDestination{"192.168.61.20", 2576}, ready: `{"host":"192.168.61.20","port":2576}`, want: &reportDestination{"192.168.61.20", 2576}},
		{name: "Telrad cannot redirect a pin", pin: &reportDestination{"192.168.61.20", 2576}, ready: `{"host":"192.168.61.21","port":2576}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := pairedTestConfig(t.TempDir())
			cfg.ReportHost, cfg.ReportPort = "", 0
			if test.pin != nil {
				cfg.ReportHost, cfg.ReportPort = test.pin.Host, test.pin.Port
			}
			cfg.ReportDestinationAllowedCIDRs = test.allowed
			applyReadyReportDestination(cfg, json.RawMessage(test.ready))
			got, err := effectiveReportDestination(cfg)
			if test.want == nil {
				if err == nil || !errors.Is(err, retrieval.ErrPolicy) {
					t.Fatalf("destination=%+v err=%v, want a policy refusal", got, err)
				}
				return
			}
			if err != nil || got != *test.want {
				t.Fatalf("destination=%+v err=%v, want %+v", got, err, *test.want)
			}
		})
	}
}

func TestReportDestinationEnvironmentAndConfig(t *testing.T) {
	t.Setenv("TELRAD_RELAY_REPORT_DESTINATION_ALLOWED_CIDRS", " 10.1.0.0/16, ,172.20.0.0/16 ")
	cfg := pairedTestConfig(t.TempDir())
	if err := applyReportDestinationEnvironment(cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.ReportDestinationAllowedCIDRs, ",") != "10.1.0.0/16,172.20.0.0/16" {
		t.Fatalf("allowed networks=%v", cfg.ReportDestinationAllowedCIDRs)
	}
	cfg.ReportHost, cfg.ReportPort = "", 0
	if err := validateConfig(cfg, "run"); err != nil {
		t.Fatalf("an unpinned Relay must be valid: %v", err)
	}
	if describeReportDestination(cfg) != "from Telrad" {
		t.Fatal(describeReportDestination(cfg))
	}
}

// mllpReceiver accepts reports on loopback and acknowledges each with AA.
func mllpReceiver(t *testing.T) (int, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if frame, err := readMLLPFrame(conn, 1024*1024); err == nil {
				received <- string(frame)
				_, _ = conn.Write([]byte("\x0bMSH|^~\\&|RIS|TEST|TELRAD|TEST|20260906000000||ACK|ack-1|P|2.5\rMSA|AA|report-1\r\x1c\r"))
			}
			_ = conn.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, received
}

func readyDestination(host string, port int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"host":%q,"port":%d}`, host, port))
}

func TestTelradReportDestinationDeliveryAndPermitInvalidation(t *testing.T) {
	first, firstReceived := mllpReceiver(t)
	second, secondReceived := mllpReceiver(t)
	payload := reportTestPayload("ACC")

	// A permit is signed for the destination in effect when the order arrived.
	cfg := pairedTestConfig(t.TempDir())
	cfg.ReportHost, cfg.ReportPort = "127.0.0.1", first
	authorization := reportTestSigner(t, cfg)(payload)
	cfg.ReportHost, cfg.ReportPort = "", 0
	applyReadyReportDestination(cfg, readyDestination("127.0.0.1", first))
	result := deliverReport(context.Background(), cfg, reportTestMessage(payload, authorization))
	if result.Outcome != "accepted" || len(firstReceived) != 1 {
		t.Fatalf("delivery to the Telrad destination: %+v", result)
	}

	// Telrad moves the receiver: earlier permits no longer authorize delivery.
	applyReadyReportDestination(cfg, readyDestination("127.0.0.1", second))
	result = deliverReport(context.Background(), cfg, reportTestMessage(payload, authorization))
	if result.Outcome != "failed" || result.Error != "invalid_report" || len(secondReceived) != 0 {
		t.Fatalf("stale permit after destination change: %+v", result)
	}

	// Telrad reports no receiver: nothing is sent.
	applyReadyReportDestination(cfg, json.RawMessage("null"))
	if result = deliverReport(context.Background(), cfg, reportTestMessage(payload, authorization)); result.Outcome != "failed" {
		t.Fatalf("delivery without a destination: %+v", result)
	}
}

func TestPinnedReportDestinationCannotBeRedirected(t *testing.T) {
	pinned, pinnedReceived := mllpReceiver(t)
	other, otherReceived := mllpReceiver(t)
	payload := reportTestPayload("ACC")
	cfg := pairedTestConfig(t.TempDir())
	cfg.ReportHost, cfg.ReportPort = "127.0.0.1", pinned
	authorization := reportTestSigner(t, cfg)(payload)

	applyReadyReportDestination(cfg, readyDestination("127.0.0.1", other))
	result := deliverReport(context.Background(), cfg, reportTestMessage(payload, authorization))
	if result.Outcome != "failed" || len(pinnedReceived)+len(otherReceived) != 0 {
		t.Fatalf("redirected pinned delivery: %+v", result)
	}

	// An older server that never sends the field keeps delivering to the pin.
	applyReadyReportDestination(cfg, nil)
	result = deliverReport(context.Background(), cfg, reportTestMessage(payload, authorization))
	if result.Outcome != "accepted" || len(pinnedReceived) != 1 || len(otherReceived) != 0 {
		t.Fatalf("pinned delivery with an older server: %+v", result)
	}
}

func TestControlSessionAdvertisesAndAppliesReportDestination(t *testing.T) {
	stateDirectory := t.TempDir()
	cfg := pairedTestConfig(stateDirectory)
	cfg.ReportHost, cfg.ReportPort = "", 0
	provider := testProvider(t, testCredential('A'))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	advertised := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/sessions"):
			var hello struct {
				Capabilities map[string]any `json:"capabilities"`
			}
			if err := json.NewDecoder(r.Body).Decode(&hello); err != nil {
				t.Error(err)
			}
			mu.Lock()
			advertised = hello.Capabilities["reportDestination"] == true
			mu.Unlock()
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(readyMessage{Type: "ready", SessionID: "session-1", ConnectorID: cfg.RelayID, IngestMode: "PRODUCTION", Transports: map[string]readyTransport{"dicom": {URL: cfg.DicomURL, ContentType: "application/dicom"}, "hl7": {URL: cfg.HL7URL, ContentType: "application/hl7-v2"}}, ReportDestination: readyDestination("192.168.61.20", 2576)})
		case strings.HasSuffix(r.URL.Path, "/poll"):
			cancel()
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg.configPath = filepath.Join(stateDirectory, "relay.json")
	setRetrievalCloud(cfg, server.URL)
	saveRetrievalTestConfig(t, cfg)
	workCtx, stopWork := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopWork()
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseControl(ctx, workCtx, cfg, server.Client(), provider, make(chan struct{}), newWorkDrainer(), newRuntimeStatus(cfg.configPath))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("control session did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if !advertised {
		t.Fatal("hello did not advertise reportDestination")
	}
	if got, err := effectiveReportDestination(cfg); err != nil || got != (reportDestination{"192.168.61.20", 2576}) {
		t.Fatalf("ready destination not applied: %+v %v", got, err)
	}
}
