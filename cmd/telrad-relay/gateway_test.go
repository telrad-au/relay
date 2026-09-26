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
	if !cfg.gatewayMode() || cfg.ReportHost != "" || cfg.ReportPort != 0 || cfg.MaxConnectionsPerPeer != defaultMaxConnectionsPerPeer || cfg.MaxConcurrentDeliveries != defaultMaxConcurrentDeliveries {
		t.Fatalf("gateway defaults: mode=%q report=%q:%d perPeer=%d deliveries=%d", cfg.Mode, cfg.ReportHost, cfg.ReportPort, cfg.MaxConnectionsPerPeer, cfg.MaxConcurrentDeliveries)
	}
	if cfg.DeliveryListenAddress != "172.19.0.1" || cfg.DeliveryPort != 2580 || cfg.DeliveryTokenPath != "delivery-token" || gatewayConfigRelative(cfg, cfg.DeliveryTokenPath) != filepath.Join(filepath.Dir(cfg.configPath), "delivery-token") {
		t.Fatalf("gateway delivery listener: %s:%d token=%q", cfg.DeliveryListenAddress, cfg.DeliveryPort, cfg.DeliveryTokenPath)
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
		"per-peer limit above global":    func(f map[string]any) { f["maxConnectionsPerPeer"] = 257 },
		"negative per-peer limit":        func(f map[string]any) { f["maxConnectionsPerPeer"] = -1 },
		"missing relay ID":               func(f map[string]any) { delete(f, "relayId") },
		"missing HL7 URL":                func(f map[string]any) { delete(f, "hl7Url") },
		"missing delivery address":       func(f map[string]any) { delete(f, "deliveryListenAddress") },
		"unspecified delivery address":   func(f map[string]any) { f["deliveryListenAddress"] = "0.0.0.0" },
		"IPv6 delivery address":          func(f map[string]any) { f["deliveryListenAddress"] = "::1" },
		"hostname delivery address":      func(f map[string]any) { f["deliveryListenAddress"] = "gateway.internal" },
		"non-canonical delivery address": func(f map[string]any) { f["deliveryListenAddress"] = "172.019.0.1" },
		"missing delivery port":          func(f map[string]any) { delete(f, "deliveryPort") },
		"oversize delivery port":         func(f map[string]any) { f["deliveryPort"] = 65536 },
		"delivery port shared with HL7":  func(f map[string]any) { f["deliveryPort"] = f["hl7Port"] },
		"missing delivery token path":    func(f map[string]any) { delete(f, "deliveryTokenPath") },
		"delivery token is credential":   func(f map[string]any) { f["deliveryTokenPath"] = f["credentialPath"] },
		"delivery token is config":       func(f map[string]any) { f["deliveryTokenPath"] = "relay.json" },
		"negative concurrent deliveries": func(f map[string]any) { f["maxConcurrentDeliveries"] = -1 },
		"too many concurrent deliveries": func(f map[string]any) { f["maxConcurrentDeliveries"] = 257 },
		"TLS certificate without key":    func(f map[string]any) { f["deliveryTlsCertPath"] = "delivery.crt" },
		"TLS key without certificate":    func(f map[string]any) { f["deliveryTlsKeyPath"] = "delivery.key" },
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
	for name, value := range map[string]any{
		"deliveryListenAddress": "172.19.0.1", "deliveryPort": 2580, "deliveryTokenPath": "delivery-token",
		"maxConcurrentDeliveries": 16, "deliveryTlsCertPath": "delivery.crt", "deliveryTlsKeyPath": "delivery.key",
	} {
		candidate := readPackagedConfigFields(t, "relay.example.json")
		candidate[name] = value
		loaded, err := loadConfig(writeGatewayTestConfig(t, candidate))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateConfig(loaded, "run"); err == nil || !strings.Contains(err.Error(), "gateway mode") {
			t.Fatalf("clinic %s error = %v", name, err)
		}
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

type gatewayObservation struct {
	method string
	path   string
	peers  []string
	body   []byte
}

// gatewayTestPlatform records every HTTPS request a running gateway makes.
// It serves HL7 ingest only: a gateway has no control session.
type gatewayTestPlatform struct {
	mu           sync.Mutex
	observations []gatewayObservation
}

func (platform *gatewayTestPlatform) snapshot() []gatewayObservation {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	return append([]gatewayObservation(nil), platform.observations...)
}

// startTestGateway runs the gateway from the packaged example with loopback
// listeners and returns its configuration, config path and delivery token.
func startTestGateway(t *testing.T) (*config, string, string, *gatewayTestPlatform) {
	t.Helper()
	platform := &gatewayTestPlatform{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		platform.mu.Lock()
		platform.observations = append(platform.observations, gatewayObservation{request.Method, request.URL.Path, request.Header.Values(gatewayPeerHeader), body})
		platform.mu.Unlock()
		if request.URL.Path != "/v1/relay/ingest/hl7" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		controlID, err := hl7ControlID(body)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/hl7-v2")
		_, _ = writer.Write(syntheticHL7Acknowledgement("AA", controlID, "ack-"+controlID))
	}))
	t.Cleanup(server.Close)
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
	fields["deliveryListenAddress"] = "127.0.0.1"
	fields["deliveryPort"] = freeLoopbackPort(t)
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
	token := writeDeliveryTestToken(t, gatewayConfigRelative(cfg, cfg.DeliveryTokenPath))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runClinicalWithContext(ctx, cfg, configPath) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("gateway stopped with %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("gateway did not stop")
		}
	})
	return cfg, configPath, token, platform
}

// Each VPN peer's orders reach Telrad attributed to that peer and nothing else.
func TestGatewayAttributesEachPeersHTTPSRequestsToItsOwnAddress(t *testing.T) {
	cfg, _, _, platform := startTestGateway(t)

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

	attributed := map[string]string{}
	for _, seen := range platform.snapshot() {
		if seen.path != "/v1/relay/ingest/hl7" {
			t.Fatalf("gateway made an unexpected request %s %s", seen.method, seen.path)
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
	listeners := make([]net.Listener, 0, 3)
	for range 3 {
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
	cfg.DeliveryListenAddress, cfg.DeliveryPort, cfg.DeliveryTokenPath, cfg.MaxConcurrentDeliveries = "127.0.0.1", listeners[2].Addr().(*net.TCPAddr).Port, "delivery-token", defaultMaxConcurrentDeliveries
	gatewayTestCredential(t, cfg.CredentialPath, "https://ingest.dev.app.telrad.com.au")
	writeDeliveryTestToken(t, gatewayConfigRelative(cfg, cfg.DeliveryTokenPath))
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
	_ = listeners[2].Close()
	if err := runtimeReady(cfg, cfg.configPath); err == nil {
		t.Fatal("gateway was ready without its delivery listener")
	}
	_ = listeners[1].Close()
	if err := runtimeReady(cfg, cfg.configPath); err == nil {
		t.Fatal("gateway was ready without its HL7 listener")
	}
}
