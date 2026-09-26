package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeGatewayTestConfig(t *testing.T, fields map[string]any) string {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "relay.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readPackagedConfigFields(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", name))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestGatewayExampleConfigurationContract(t *testing.T) {
	fields := readPackagedConfigFields(t, "relay.gateway.example.json")
	cfg, err := loadConfig(writeGatewayTestConfig(t, fields))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"run", "doctor", "ready", "rotate-credential"} {
		if err := validateConfig(cfg, command); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	if !cfg.gatewayMode() || cfg.ReportHost != "" || cfg.ReportPort != 0 || cfg.MaxConnectionsPerPeer != defaultMaxConnectionsPerPeer {
		t.Fatalf("gateway defaults: mode=%q report=%q:%d perPeer=%d", cfg.Mode, cfg.ReportHost, cfg.ReportPort, cfg.MaxConnectionsPerPeer)
	}
	for _, command := range []string{"auth", "enroll"} {
		if err := validateConfig(cfg, command); err == nil || !strings.Contains(err.Error(), "does not pair") {
			t.Fatalf("%s error = %v", command, err)
		}
	}

	rejected := map[string]func(map[string]any){
		"unknown mode":            func(f map[string]any) { f["mode"] = "hosted" },
		"retrieval":               func(f map[string]any) { f["retrieval"] = map[string]any{} },
		"disabled DICOM listener": func(f map[string]any) { f["disableDicomListener"] = true },
		"report host":             func(f map[string]any) { f["reportHost"] = "127.0.0.1" },
		"report port":             func(f map[string]any) { f["reportPort"] = 2576 },
		"update manifest": func(f map[string]any) {
			f["updateManifestUrl"] = "https://example.test/stable.json"
			f["updatePublicKey"] = "not-used"
		},
		"per-peer limit above global": func(f map[string]any) { f["maxConnectionsPerPeer"] = 257 },
		"negative per-peer limit":     func(f map[string]any) { f["maxConnectionsPerPeer"] = -1 },
		"missing relay ID":            func(f map[string]any) { delete(f, "relayId") },
		"missing HL7 URL":             func(f map[string]any) { delete(f, "hl7Url") },
	}
	for name, mutate := range rejected {
		t.Run(name, func(t *testing.T) {
			candidate := readPackagedConfigFields(t, "relay.gateway.example.json")
			mutate(candidate)
			loaded, err := loadConfig(writeGatewayTestConfig(t, candidate))
			if err == nil {
				err = validateConfig(loaded, "run")
			}
			if err == nil {
				t.Fatal("invalid gateway configuration was accepted")
			}
		})
	}
	t.Run("report destination environment", func(t *testing.T) {
		t.Setenv("TELRAD_RELAY_REPORT_DESTINATION_HOST", "127.0.0.1")
		loaded, err := loadConfig(writeGatewayTestConfig(t, fields))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateConfig(loaded, "run"); err == nil {
			t.Fatal("gateway accepted a configured report destination")
		}
	})
}

func TestClinicConfigurationIsUnchangedByGatewayMode(t *testing.T) {
	fields := readPackagedConfigFields(t, "relay.example.json")
	cfg, err := loadConfig(writeGatewayTestConfig(t, fields))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(cfg, "run"); err != nil {
		t.Fatal(err)
	}
	if cfg.gatewayMode() || cfg.ReportHost != "127.0.0.1" || cfg.ReportPort != 2576 || cfg.MaxConnectionsPerPeer != 0 {
		t.Fatalf("clinic configuration changed: %+v", cfg)
	}
	fields["mode"] = "clinic"
	explicit, err := loadConfig(writeGatewayTestConfig(t, fields))
	if err != nil || validateConfig(explicit, "run") != nil || explicit.gatewayMode() {
		t.Fatalf("explicit clinic mode: %v", err)
	}
	fields["maxConnectionsPerPeer"] = 8
	limited, err := loadConfig(writeGatewayTestConfig(t, fields))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(limited, "run"); err == nil || !strings.Contains(err.Error(), "gateway mode") {
		t.Fatalf("clinic per-peer limit error = %v", err)
	}
	// A clinic Relay accepts and ignores a claim destination.
	var report reportMessage
	claim := `{"type":"report","deliveryId":"d","token":"t","messageControlId":"m","payload":"p","payloadSha256":"s","claimExpiresAt":"2026-01-01T00:00:00Z","authorization":"a","destination":{"host":"100.100.0.42","port":2575}}`
	if err := decodeBoundedJSON(strings.NewReader(claim), maxCloudResponseBytes, &report); err != nil || report.Destination == nil || report.Destination.Host != "100.100.0.42" || report.Destination.Port != 2575 {
		t.Fatalf("claim destination = %+v error=%v", report.Destination, err)
	}
}

func gatewayTestCredential(t *testing.T, path, origin string) {
	t.Helper()
	now := time.Now().UTC()
	record := credentialFile{
		SchemaVersion: credentialLifecycleSchemaVersion, CredentialVersion: 2, FamilyID: "gateway-family", Generation: 1,
		AccessCredential: testAccessCredential('G'), AccessObtainedAt: now, AccessExpiresAt: now.Add(time.Hour),
		RenewableCredential: testRenewableCredential('G'), RenewableExpiresAt: now.Add(24 * time.Hour),
		RenewalURL: origin + "/v1/relay/credentials/renew",
	}
	if err := commitCredential(path, record); err != nil {
		t.Fatal(err)
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// Each VPN peer's orders reach Telrad attributed to that peer and nothing else.
// Control traffic is the gateway's own and carries no peer address.
func TestGatewayAttributesEachPeersHTTPSRequestsToItsOwnAddress(t *testing.T) {
	type observation struct {
		path  string
		peers []string
		body  []byte
	}
	var mu sync.Mutex
	var observations []observation
	hello := make(chan map[string]any, 1)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		observations = append(observations, observation{request.URL.Path, request.Header.Values(gatewayPeerHeader), body})
		mu.Unlock()
		switch {
		case request.URL.Path == "/v1/relay/ingest/hl7":
			controlID, err := hl7ControlID(body)
			if err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "application/hl7-v2")
			_, _ = writer.Write(syntheticHL7Acknowledgement("AA", controlID, "ack-"+controlID))
		case request.Method == http.MethodPost && request.URL.Path == "/v1/relay/control/sessions":
			var message map[string]any
			_ = json.Unmarshal(body, &message)
			select {
			case hello <- message:
			default:
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"type":"ready","sessionId":"gateway-session","connectorId":"gateway-relay","ingestMode":"PRODUCTION","transports":{"dicom":{"url":%q,"contentType":"application/dicom"},"hl7":{"url":%q,"contentType":"application/hl7-v2"}}}`,
				server.URL+"/v1/relay/ingest/dicom", server.URL+"/v1/relay/ingest/hl7")
		case strings.HasSuffix(request.URL.Path, "/poll") || request.Method == http.MethodDelete:
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	oldFactory := clientFactory
	clientFactory = func(*config) protocolClients {
		return protocolClients{secure: server.Client(), updates: server.Client()}
	}
	t.Cleanup(func() { clientFactory = oldFactory })

	directory := t.TempDir()
	fields := readPackagedConfigFields(t, "relay.gateway.example.json")
	for name, path := range map[string]string{"pairingUrl": "/v1/relay/pairing-enrollments", "controlUrl": "/v1/relay/control", "dicomUrl": "/v1/relay/ingest/dicom", "hl7Url": "/v1/relay/ingest/hl7"} {
		fields[name] = server.URL + path
	}
	fields["listenAddress"] = "127.0.0.1"
	fields["dicomPort"] = freeLoopbackPort(t)
	fields["hl7Port"] = freeLoopbackPort(t)
	data, _ := json.Marshal(fields)
	configPath := filepath.Join(directory, "relay.json")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(cfg, "run"); err != nil {
		t.Fatal(err)
	}
	gatewayTestCredential(t, cfg.CredentialPath, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runClinicalWithContext(ctx, cfg, configPath) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("gateway did not stop")
		}
	})

	select {
	case message := <-hello:
		capabilities, _ := message["capabilities"].(map[string]any)
		if capabilities["gateway"] != true || capabilities["dicom"] != true || capabilities["hl7"] != true {
			t.Fatalf("hello capabilities = %v", capabilities)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not open a control session")
	}

	hl7Address := net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.HL7Port))
	sendFrom := func(peer, controlID string) error {
		var dialer net.Dialer
		var conn net.Conn
		var err error
		for attempt := 0; attempt < 50; attempt++ {
			dialer = net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(peer)}}
			if conn, err = dialer.Dial("tcp4", hl7Address); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		frame := append([]byte{mllpStart}, syntheticHL7IntegrityMessage(controlID)...)
		if _, err := conn.Write(append(frame, mllpEnd, mllpCR)); err != nil {
			return err
		}
		response, err := readMLLPFrame(conn, 1<<20)
		if err != nil {
			return err
		}
		if _, receivedID, err := parseHL7Acknowledgement(response[1 : len(response)-2]); err != nil || receivedID != controlID {
			return fmt.Errorf("ACK for %s = %q (%v)", controlID, response, err)
		}
		return nil
	}
	peers := map[string]string{"127.0.0.2": "peer-a-order", "127.0.0.3": "peer-b-order"}
	var wg sync.WaitGroup
	errs := make(chan error, len(peers))
	for peer, controlID := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sendFrom(peer, controlID); err != nil {
				errs <- fmt.Errorf("%s: %w", peer, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if strings.Contains(err.Error(), "assign requested address") {
			t.Skipf("loopback aliases are unavailable in this sandbox: %v", err)
		}
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	attributed := map[string]string{}
	for _, seen := range observations {
		if seen.path != "/v1/relay/ingest/hl7" {
			if len(seen.peers) != 0 {
				t.Fatalf("gateway control request %s carried a peer address %q", seen.path, seen.peers)
			}
			continue
		}
		if len(seen.peers) != 1 {
			t.Fatalf("HL7 request carried %d peer header values: %q", len(seen.peers), seen.peers)
		}
		controlID, err := hl7ControlID(seen.body)
		if err != nil {
			t.Fatal(err)
		}
		attributed[controlID] = seen.peers[0]
	}
	for peer, controlID := range peers {
		if attributed[controlID] != peer {
			t.Fatalf("order %s attributed to %q, want %q (all: %v)", controlID, attributed[controlID], peer, attributed)
		}
	}
}

func TestGatewayPeerAttributionRejectsNonIPv4AndCapsEachPeer(t *testing.T) {
	for _, test := range []struct {
		address net.Addr
		want    string
	}{
		{&net.TCPAddr{IP: net.ParseIP("100.96.0.10"), Port: 40000}, "100.96.0.10"},
		{&net.TCPAddr{IP: net.ParseIP("::ffff:100.96.0.11"), Port: 40000}, "100.96.0.11"},
		{&net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 40000}, ""},
		{&net.TCPAddr{IP: net.IPv4zero, Port: 40000}, ""},
		{&net.UnixAddr{Name: "/tmp/socket", Net: "unix"}, ""},
	} {
		peer, ok := gatewayPeer(test.address)
		if test.want == "" && ok || test.want != "" && (!ok || peer.String() != test.want) {
			t.Fatalf("%v: peer=%v ok=%t", test.address, peer, ok)
		}
	}

	gateway := &config{Mode: relayModeGateway, MaxConnectionsPerPeer: 1}
	attribution := newConnectionAttribution(gateway, &http.Client{})
	var served atomic.Int32
	release := make(chan struct{})
	handler := attribution.serve(func(net.Conn, *http.Client) { served.Add(1); <-release })
	pipeClinic, pipeRelay := net.Pipe()
	defer pipeClinic.Close()
	handler(pipeRelay) // net.Pipe has no IPv4 peer and is never served.
	if served.Load() != 0 {
		t.Fatal("non-IPv4 peer reached a protocol handler")
	}
	peer := netip.MustParseAddr("100.96.0.10")
	if !attribution.acquire(peer) || attribution.acquire(peer) || !attribution.acquire(netip.MustParseAddr("100.96.0.11")) {
		t.Fatal("per-peer cap did not isolate peers")
	}
	attribution.release(peer)
	if !attribution.acquire(peer) {
		t.Fatal("released per-peer slot was not reusable")
	}
	close(release)
}

func TestPeerTransportSetsExactlyOneHeaderWithoutMutatingTheRequest(t *testing.T) {
	var seen []string
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request.Header.Values(gatewayPeerHeader)
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: request}, nil
	})
	client := peerClient(&http.Client{Transport: base}, netip.MustParseAddr("100.96.0.10"))
	request, _ := http.NewRequest(http.MethodPost, "https://example.test/v1/relay/ingest/hl7", bytes.NewReader([]byte("x")))
	request.Header.Add(gatewayPeerHeader, "100.96.0.99")
	request.Header.Add(gatewayPeerHeader, "100.96.0.98")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if len(seen) != 1 || seen[0] != "100.96.0.10" {
		t.Fatalf("peer header = %q", seen)
	}
	if values := request.Header.Values(gatewayPeerHeader); len(values) != 2 {
		t.Fatalf("caller request was mutated: %q", values)
	}
}

func fakeRIS(t *testing.T) (int, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var connections atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, _ = readMLLPFrame(conn, 1<<20)
			_, _ = conn.Write([]byte("\x0bMSH|^~\\&|RIS|TEST|TELRAD|TEST|20260910000000||ACK|ack|P|2.5\rMSA|AA|report-1\r\x1c\r"))
			_ = conn.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, &connections
}

func TestGatewayReportDeliveryDialsTheClaimedDestination(t *testing.T) {
	port, connections := fakeRIS(t)
	cfg := pairedTestConfig(t.TempDir())
	cfg.Mode, cfg.ReportHost, cfg.ReportPort, cfg.MaxConnectionsPerPeer = relayModeGateway, "", 0, defaultMaxConnectionsPerPeer
	cfg.configPath = ""
	if err := validateConfig(cfg, "run"); err != nil {
		t.Fatal(err)
	}
	good := reportTestMessage(reportTestPayload("ACC"), "")
	good.Destination = &reportDestination{Host: "127.0.0.1", Port: port}
	if result := deliverReport(context.Background(), cfg, good); result.Outcome != "accepted" || result.AckCode != "AA" {
		t.Fatalf("gateway delivery: %+v", result)
	}
	if connections.Load() != 1 {
		t.Fatalf("RIS connections = %d", connections.Load())
	}
	for name, mutate := range map[string]func(*reportMessage){
		"missing destination":     func(r *reportMessage) { r.Destination = nil },
		"hostname destination":    func(r *reportMessage) { r.Destination = &reportDestination{Host: "localhost", Port: port} },
		"IPv6 destination":        func(r *reportMessage) { r.Destination = &reportDestination{Host: "::1", Port: port} },
		"mapped IPv6 destination": func(r *reportMessage) { r.Destination = &reportDestination{Host: "::ffff:127.0.0.1", Port: port} },
		"non-canonical IPv4":      func(r *reportMessage) { r.Destination = &reportDestination{Host: "127.000.000.001", Port: port} },
		"unspecified destination": func(r *reportMessage) { r.Destination = &reportDestination{Host: "0.0.0.0", Port: port} },
		"zero port":               func(r *reportMessage) { r.Destination.Port = 0 },
		"oversize port":           func(r *reportMessage) { r.Destination.Port = 65536 },
		"order injection":         func(r *reportMessage) { r.Payload = strings.Replace(r.Payload, "ORU^R01", "ORM^O01", 1) },
		"embedded message":        func(r *reportMessage) { r.Payload += "MSH|^~\\&|X|X|X|X|20260101||ORU^R01|x|P|2.5\r" },
		"digest mismatch":         func(r *reportMessage) { r.PayloadSHA256 = strings.Repeat("0", 64) },
		"control ID mismatch":     func(r *reportMessage) { r.MessageControlID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			r := good
			r.Destination = &reportDestination{Host: good.Destination.Host, Port: good.Destination.Port}
			mutate(&r)
			if name != "digest mismatch" && name != "control ID mismatch" {
				r.PayloadSHA256 = reportTestMessage(r.Payload, "").PayloadSHA256
			}
			before := connections.Load()
			if result := deliverReport(context.Background(), cfg, r); result.Outcome != "failed" || result.Error != "invalid_report" {
				t.Fatalf("invalid claim delivered: %+v", result)
			}
			if connections.Load() != before {
				t.Fatal("invalid claim reached the RIS")
			}
		})
	}
}

func TestClinicReportDeliveryIgnoresClaimDestination(t *testing.T) {
	configuredPort, configured := fakeRIS(t)
	claimedPort, claimed := fakeRIS(t)
	cfg := retrievalTestConfig(t)
	cfg.ReportHost = "127.0.0.1"
	cfg.ReportPort = configuredPort
	saveRetrievalTestConfig(t, cfg)
	permits, err := signReportAuthorizations(cfg, retrievalTestHL7("NW", 1))
	if err != nil {
		t.Fatal(err)
	}
	report := reportTestMessage(reportTestPayload("ACC"), permits[0])
	report.Destination = &reportDestination{Host: "127.0.0.1", Port: claimedPort}
	if result := deliverReport(context.Background(), cfg, report); result.Outcome != "accepted" {
		t.Fatalf("clinic delivery: %+v", result)
	}
	if configured.Load() != 1 || claimed.Load() != 0 {
		t.Fatalf("configured=%d claimed=%d", configured.Load(), claimed.Load())
	}
	report.Authorization = ""
	if result := deliverReport(context.Background(), cfg, report); result.Outcome != "failed" || result.Error != "invalid_report" {
		t.Fatalf("clinic delivery without a permit: %+v", result)
	}
}

func TestGatewayIngestsHL7WithoutSigningOrReferral(t *testing.T) {
	var paths []string
	var contentTypes []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		paths = append(paths, request.URL.Path)
		contentTypes = append(contentTypes, request.Header.Get("Content-Type"))
		controlID, _ := hl7ControlID(body)
		writer.Header().Set("Content-Type", "application/hl7-v2")
		_, _ = writer.Write(syntheticHL7Acknowledgement("AA", controlID, "ack"))
	}))
	defer server.Close()
	cfg := pairedTestConfig(t.TempDir())
	cfg.Mode = relayModeGateway
	cfg.HL7URL = server.URL + "/v1/relay/ingest/hl7"
	// No signing key exists; a clinic Relay would fail to sign this order.
	order := retrievalTestHL7("NW", 1)
	ack, err := ingestClinicHL7(context.Background(), cfg, &net.TCPAddr{IP: net.ParseIP("100.96.0.10")}, server.Client(), testProvider(t, testCredential('A')), newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")), order, "message-1")
	if err != nil || len(ack) == 0 {
		t.Fatalf("ACK=%q error=%v", ack, err)
	}
	if len(paths) != 1 || paths[0] != "/v1/relay/ingest/hl7" || contentTypes[0] != "application/hl7-v2" {
		t.Fatalf("paths=%v contentTypes=%v", paths, contentTypes)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.CredentialPath), reportKeyFilename)); !os.IsNotExist(err) {
		t.Fatalf("gateway created a report signing key: %v", err)
	}
}

func TestGatewayReadyChecksHeldSocketsWithoutDialling(t *testing.T) {
	listeners := make([]net.Listener, 0, 2)
	for range 2 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		t.Cleanup(func() { _ = listener.Close() })
	}
	directory := t.TempDir()
	cfg := pairedTestConfig(directory)
	cfg.Mode, cfg.ReportHost, cfg.ReportPort, cfg.MaxConnectionsPerPeer = relayModeGateway, "", 0, defaultMaxConnectionsPerPeer
	cfg.ListenAddress = "127.0.0.1"
	cfg.DicomPort = listeners[0].Addr().(*net.TCPAddr).Port
	cfg.HL7Port = listeners[1].Addr().(*net.TCPAddr).Port
	gatewayTestCredential(t, cfg.CredentialPath, "https://ingest.dev.app.telrad.com.au")
	newRuntimeStatus(cfg.configPath).SetIngestReady(true)
	if err := validateConfig(cfg, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := runtimeReady(cfg, cfg.configPath); err != nil {
		t.Fatalf("held gateway listeners were not ready: %v", err)
	}
	var doctorOutput bytes.Buffer
	if err := doctorTo(cfg, &doctorOutput); err != nil || !strings.Contains(doctorOutput.String(), "gateway") {
		t.Fatalf("doctor output=%q error=%v", doctorOutput.String(), err)
	}
	_ = listeners[1].Close()
	if err := runtimeReady(cfg, cfg.configPath); err == nil {
		t.Fatal("gateway was ready without its HL7 listener")
	}
}
