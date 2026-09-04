package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestPairing201DerivesEndpointsAuthorizationAndPersistsAtomically(t *testing.T) {
	credential := testCredential('P')
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/relay/pairing-enrollments" || request.Method != http.MethodPost {
			t.Fatalf("request %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("pairing request unexpectedly carried authorization")
		}
		body, _ := io.ReadAll(request.Body)
		if !bytes.Contains(body, []byte(`"pairingToken":"`+strings.Repeat("t", 40)+`"`)) {
			t.Fatal("pairing token missing")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(writer, `{"relayId":"relay-1","credential":%q,"protocolVersion":1}`, credential)
	}))
	defer server.Close()
	oldFactory := clientFactory
	clientFactory = func(cfg *config) protocolClients {
		return protocolClients{secure: server.Client(), updates: server.Client()}
	}
	t.Cleanup(func() { clientFactory = oldFactory })
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	cfg := defaultConfig()
	cfg.configPath = configPath
	cfg.PairingURL = server.URL + "/v1/relay/pairing-enrollments"
	cfg.CredentialPath = filepath.Join(directory, "relay-credential.json")
	cfg.credentialPathConfigured = "relay-credential.json"
	initial, _ := jsonMarshalIndent(cfg)
	if err := os.WriteFile(configPath, initial, 0600); err != nil {
		t.Fatal(err)
	}
	token := []byte(strings.Repeat("t", 40))
	if err := enrollWithPairingToken(context.Background(), cfg, configPath, token); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(configPath)
	wantControlURL := strings.Replace(server.URL, "https://", "wss://", 1) + "/v1/relay/control"
	if err != nil || loaded.RelayID != "relay-1" || loaded.ControlURL != wantControlURL ||
		loaded.DicomURL != server.URL+"/v1/relay/ingest/dicom" || loaded.HL7URL != server.URL+"/v1/relay/ingest/hl7" {
		t.Fatalf("loaded=%#v error=%v", loaded, err)
	}
	persistedConfig, _ := os.ReadFile(configPath)
	if !bytes.Contains(persistedConfig, []byte(`"credentialPath": "relay-credential.json"`)) {
		t.Fatal("pairing did not preserve the configured relative credential path")
	}
	record, err := readCredentialFile(cfg.CredentialPath, time.Now())
	if err != nil || record.Credential != credential {
		t.Fatalf("credential persisted=%t error=%v", record.Credential == credential, err)
	}
}

func TestPairingLegacyEndpointsAreCompatibilityOnly(t *testing.T) {
	pairingURL := "https://ingest.example.test/v1/relay/pairing-enrollments"
	derived, err := deriveProtocolEndpoints(pairingURL)
	if err != nil {
		t.Fatal(err)
	}
	valid := pairingResponse{
		RelayID:          "relay-legacy",
		Credential:       testCredential('L'),
		ProtocolVersion:  1,
		LegacyPairingURL: derived.PairingURL,
		LegacyControlURL: derived.ControlURL,
		LegacyDicomURL:   derived.DicomURL,
		LegacyHL7URL:     derived.HL7URL,
	}
	if endpoints, err := validatePairingResponse(pairingURL, valid); err != nil || endpoints != derived {
		t.Fatalf("matching legacy endpoints=%#v error=%v", endpoints, err)
	}

	mismatched := valid
	mismatched.LegacyDicomURL = "https://other.example.test/v1/relay/ingest/dicom"
	if _, err := validatePairingResponse(pairingURL, mismatched); err == nil || !strings.Contains(err.Error(), "endpoint_mismatch") {
		t.Fatalf("mismatched legacy endpoint error=%v", err)
	}

	partial := valid
	partial.LegacyHL7URL = ""
	if _, err := validatePairingResponse(pairingURL, partial); err == nil || !strings.Contains(err.Error(), "invalid_response") {
		t.Fatalf("partial legacy endpoint error=%v", err)
	}
}

func TestNativeDeviceAuthorizationDisplaysApprovalFlowAndRedeemsPairingToken(t *testing.T) {
	credential := testCredential('D')
	pairingToken := strings.Repeat("t", 40)
	deviceSecret := strings.Repeat("s", 40)
	var requests []string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "" || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("invalid device authorization request metadata for %s", request.URL.Path)
		}
		switch request.URL.Path {
		case "/v1/relay/device-authorizations":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["hostname"] == "" || body["platform"] == "" || body["agentVersion"] != version {
				t.Fatalf("device metadata=%#v", body)
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"requestId":"request-1","deviceSecret":%q,"verificationUri":"https://app.example.test/relay-authorization/request-1","expiresAt":%q,"intervalSeconds":5}`,
				deviceSecret, time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339))
		case "/v1/relay/device-authorizations/request-1/token":
			body, _ := io.ReadAll(request.Body)
			if !bytes.Contains(body, []byte(`"deviceSecret":"`+deviceSecret+`"`)) {
				t.Fatal("device authorization poll omitted its secret")
			}
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = fmt.Fprintf(writer, `{"pairingToken":%q}`, pairingToken)
		case "/v1/relay/pairing-enrollments":
			body, _ := io.ReadAll(request.Body)
			if !bytes.Contains(body, []byte(`"pairingToken":"`+pairingToken+`"`)) {
				t.Fatal("approved pairing token was not redeemed")
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"relayId":"relay-native","credential":%q,"protocolVersion":1,"pairingUrl":%q,"controlUrl":%q,"dicomUrl":%q,"hl7Url":%q}`,
				credential, server.URL+"/v1/relay/pairing-enrollments", strings.Replace(server.URL, "https://", "wss://", 1)+"/v1/relay/control", server.URL+"/v1/relay/ingest/dicom", server.URL+"/v1/relay/ingest/hl7")
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	oldFactory := clientFactory
	clientFactory = func(cfg *config) protocolClients {
		return protocolClients{secure: server.Client(), updates: server.Client()}
	}
	t.Cleanup(func() { clientFactory = oldFactory })
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	cfg := defaultConfig()
	cfg.configPath = configPath
	cfg.PairingURL = server.URL + "/v1/relay/pairing-enrollments"
	cfg.CredentialPath = filepath.Join(directory, "relay-credential.json")
	cfg.credentialPathConfigured = "relay-credential.json"
	initial, _ := jsonMarshalIndent(cfg)
	if err := os.WriteFile(configPath, initial, 0600); err != nil {
		t.Fatal(err)
	}
	if err := enrollWithDeviceAuthorization(context.Background(), cfg, configPath); err != nil {
		t.Fatal(err)
	}
	if strings.Join(requests, ",") != "/v1/relay/device-authorizations,/v1/relay/device-authorizations/request-1/token,/v1/relay/pairing-enrollments" {
		t.Fatalf("request sequence=%v", requests)
	}
	loaded, err := loadConfig(configPath)
	if err != nil || loaded.RelayID != "relay-native" {
		t.Fatalf("loaded=%#v error=%v", loaded, err)
	}
}

func TestNativeDeviceAuthorizationReportsDenial(t *testing.T) {
	deviceSecret := strings.Repeat("s", 40)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/relay/device-authorizations" {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"requestId":"denied-1","deviceSecret":%q,"verificationUri":"https://app.example.test/relay-authorization/denied-1","expiresAt":%q,"intervalSeconds":5}`,
				deviceSecret, time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339))
			return
		}
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	oldFactory := clientFactory
	clientFactory = func(cfg *config) protocolClients {
		return protocolClients{secure: server.Client(), updates: server.Client()}
	}
	t.Cleanup(func() { clientFactory = oldFactory })
	cfg := defaultConfig()
	cfg.PairingURL = server.URL + "/v1/relay/pairing-enrollments"
	if _, err := authorizeNativeDevice(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denial error=%v", err)
	}
}

func TestCredentialRotationSendsEmptyBearerPostAndStoresOverlap(t *testing.T) {
	oldCredential := testCredential('O')
	newCredential := testCredential('N')
	deadline := time.Now().UTC().Add(15 * time.Minute).Truncate(time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/relay/credentials/rotate" || len(body) != 0 {
			t.Fatalf("rotation request %s %s body=%q", request.Method, request.URL.Path, body)
		}
		if request.Header.Get("Authorization") != "Bearer "+oldCredential || request.Header.Get("Content-Type") != "" {
			t.Fatal("rotation authentication or empty-body metadata is invalid")
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(writer, `{"credential":%q,"oldCredentialValidUntil":%q}`, newCredential, deadline.Format(time.RFC3339))
	}))
	defer server.Close()
	oldFactory := clientFactory
	clientFactory = func(cfg *config) protocolClients {
		return protocolClients{secure: server.Client(), updates: server.Client()}
	}
	t.Cleanup(func() { clientFactory = oldFactory })
	directory := t.TempDir()
	cfg := pairedTestConfig(directory)
	cfg.PairingURL = server.URL + "/v1/relay/pairing-enrollments"
	if err := commitCredential(cfg.CredentialPath, credentialFile{SchemaVersion: 1, Credential: oldCredential}); err != nil {
		t.Fatal(err)
	}
	if err := rotateCredential(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	record, err := readCredentialFile(cfg.CredentialPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if record.Credential != newCredential || record.PreviousCredential != oldCredential || record.PreviousValidUntil == nil || !record.PreviousValidUntil.Equal(deadline) {
		t.Fatalf("rotation record=%#v", record)
	}
}

func TestControlUsesBearerHelloAndTrustedReadyTransports(t *testing.T) {
	credential := testCredential('C')
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization := request.Header.Values("Authorization")
		if request.URL.Path != "/v1/relay/control" || len(authorization) != 1 || authorization[0] != "Bearer "+credential {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		var hello map[string]any
		if err := readJSON(request.Context(), connection, &hello); err != nil {
			return
		}
		capabilities, _ := hello["capabilities"].(map[string]any)
		if hello["type"] != "hello" || capabilities["httpsIngest"] != true {
			_ = connection.Close(websocket.StatusPolicyViolation, "invalid hello")
			return
		}
		if _, present := capabilities["directMtls"]; present {
			_ = connection.Close(websocket.StatusPolicyViolation, "legacy capability")
			return
		}
		origin := server.URL
		_ = writeJSON(request.Context(), connection, readyMessage{Type: "ready", SessionID: "session-1", Transports: map[string]readyTransport{
			"dicom": {URL: origin + "/v1/relay/ingest/dicom", ContentType: "application/dicom"},
			"hl7":   {URL: origin + "/v1/relay/ingest/hl7", ContentType: "application/hl7-v2"},
		}})
		for {
			if _, _, err := connection.Read(request.Context()); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	directory := t.TempDir()
	cfg := pairedTestConfig(directory)
	cfg.ControlURL = strings.Replace(server.URL, "https://", "wss://", 1) + "/v1/relay/control"
	cfg.DicomURL = server.URL + "/v1/relay/ingest/dicom"
	cfg.HL7URL = server.URL + "/v1/relay/ingest/hl7"
	provider := testProvider(t, credential)
	ctx, cancel := context.WithCancel(context.Background())
	status := newRuntimeStatus(filepath.Join(directory, "relay.json"))
	established, errors := connectControl(ctx, cfg, filepath.Join(directory, "relay.json"), server.Client(), provider, newWorkDrainer(), status)
	if !established {
		t.Fatalf("control failed: %v", <-errors)
	}
	status.mu.Lock()
	connected := status.status.ControlConnected && status.status.ReportReturnAvailable
	status.mu.Unlock()
	if !connected {
		t.Fatal("control status was not connected")
	}
	cancel()
	select {
	case <-errors:
	case <-time.After(time.Second):
		t.Fatal("control did not stop")
	}
}

func TestReadyValidationRejectsEndpointOrContentTypeChanges(t *testing.T) {
	cfg := pairedTestConfig(t.TempDir())
	valid := readyMessage{Type: "ready", SessionID: "session", Transports: map[string]readyTransport{
		"dicom": {URL: cfg.DicomURL, ContentType: "application/dicom"},
		"hl7":   {URL: cfg.HL7URL, ContentType: "application/hl7-v2"},
	}}
	if err := validateReady(cfg, valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Transports = map[string]readyTransport{
		"dicom": {URL: cfg.DicomURL + "?changed=1", ContentType: "application/dicom"},
		"hl7":   {URL: cfg.HL7URL, ContentType: "application/hl7-v2; charset=utf-8"},
	}
	if err := validateReady(cfg, invalid); err == nil {
		t.Fatal("changed ready transports were accepted")
	}
}

func jsonMarshalIndent(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	return append(data, '\n'), err
}

func TestHL7RetriesSameBodyAndKeyThenReturnsExactACK(t *testing.T) {
	message := []byte("MSH|^~\\&|CLINIC|A|TELRAD|B|20260101000000||ORM^O01|control-1|P|2.5\rPID|1")
	ack := []byte("MSH|^~\\&|TELRAD|B|CLINIC|A|20260101000001||ACK|ack-1|P|2.5\rMSA|AE|control-1\r")
	var mu sync.Mutex
	var bodies [][]byte
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		bodies = append(bodies, body)
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		attempt := len(bodies)
		mu.Unlock()
		if request.Header.Get("Authorization") != "Bearer "+testCredential('A') || request.Header.Get("X-Telrad-Protocol-Version") != "1" || request.Header.Get("Content-Type") != "application/hl7-v2" {
			t.Fatal("required headers missing")
		}
		if attempt < 3 {
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/hl7-v2")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(ack)
	}))
	defer server.Close()
	provider := testProvider(t, testCredential('A'))
	result, err := ingestHL7(context.Background(), server.URL, server.Client(), provider, newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")), message, "control-1")
	if err != nil || !bytes.Equal(result, ack) {
		t.Fatalf("ACK=%q error=%v", result, err)
	}
	if len(bodies) != 3 || !bytes.Equal(bodies[0], bodies[1]) || !bytes.Equal(bodies[1], bodies[2]) || keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("retry bodies/keys differ: %d %#v", len(bodies), keys)
	}
}

func TestHL7KeepsClinicConnectionForSequentialExchanges(t *testing.T) {
	var count int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		message, _ := io.ReadAll(request.Body)
		controlID, err := hl7ControlID(message)
		if err != nil {
			t.Fatal(err)
		}
		count++
		writer.Header().Set("Content-Type", "application/hl7-v2")
		fmt.Fprintf(writer, "MSA|AA|%s\r", controlID)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.HL7URL = server.URL
	provider := testProvider(t, testCredential('A'))
	clinic, relay := net.Pipe()
	defer clinic.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveHL7(ctx, relay, cfg, server.Client(), provider, newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")))
	for index := 1; index <= 2; index++ {
		id := fmt.Sprintf("control-%d", index)
		message := []byte("MSH|^~\\&|CLINIC|A|TELRAD|B|20260101000000||ORM^O01|" + id + "|P|2.5\r")
		frame := append([]byte{mllpStart}, message...)
		frame = append(frame, mllpEnd, mllpCR)
		if _, err := clinic.Write(frame); err != nil {
			t.Fatal(err)
		}
		response, err := readMLLPFrame(clinic, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(response, append(append([]byte{mllpStart}, []byte("MSA|AA|"+id+"\r")...), mllpEnd, mllpCR)) {
			t.Fatalf("response=%q", response)
		}
	}
	if count != 2 {
		t.Fatalf("cloud requests=%d", count)
	}
}

func TestHL7RejectsMalformedAcknowledgementsAndOversizeFrames(t *testing.T) {
	for name, ack := range map[string]string{
		"wrong control id": "MSA|AA|different\r",
		"unknown category": "MSA|CA|control-1\r",
		"invalid utf8":     string([]byte{'M', 'S', 'A', '|', 'A', 'A', '|', 0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/hl7-v2"}},
				Body:       io.NopCloser(strings.NewReader(ack)),
			}
			if _, err := consumeHL7Response(response, "control-1", newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json"))); err == nil {
				t.Fatal("malformed ACK was accepted")
			}
		})
	}
	frame := append([]byte{mllpStart}, bytes.Repeat([]byte{'x'}, 5)...)
	frame = append(frame, mllpEnd, mllpCR)
	if _, err := readMLLPFrame(bytes.NewReader(frame), 4); err == nil {
		t.Fatal("oversize MLLP frame was accepted")
	}
	if _, err := hl7ControlID([]byte{'M', 'S', 'H', '|', 0xff}); err == nil {
		t.Fatal("invalid UTF-8 HL7 message was accepted")
	}
}

func TestHL7RetryHonorsCancellation(t *testing.T) {
	var requests atomic.Int32
	requestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		select {
		case requestSeen <- struct{}{}:
		default:
		}
		writer.Header().Set("Retry-After", "30")
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	provider := testProvider(t, testCredential('A'))
	status := newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	message := []byte("MSH|^~\\&|A|B|C|D|20260101000000||ORM^O01|cancel-1|P|2.5\r")
	result := make(chan error, 1)
	go func() {
		_, err := ingestHL7(ctx, server.URL, server.Client(), provider, status, message, "cancel-1")
		result <- err
	}()
	select {
	case <-requestSeen:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("initial HL7 request was not observed")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled retry unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled HL7 retry did not stop")
	}
	if elapsed := time.Since(started); elapsed > time.Second || requests.Load() != 1 {
		t.Fatalf("cancellation took %s after %d requests", elapsed, requests.Load())
	}
}

func TestHL7HandlesConcurrentClinicConnections(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		message, _ := io.ReadAll(request.Body)
		controlID, err := hl7ControlID(message)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		mu.Lock()
		seen[controlID] = true
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/hl7-v2")
		_, _ = fmt.Fprintf(writer, "MSA|AA|%s\r", controlID)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.HL7URL = server.URL
	provider := testProvider(t, testCredential('A'))
	statusManagers := make([]*runtimeStatusManager, 4)
	for index := range statusManagers {
		statusManagers[index] = newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json"))
	}
	var wait sync.WaitGroup
	for index := 0; index < 4; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			clinic, relay := net.Pipe()
			defer clinic.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go serveHL7(ctx, relay, cfg, server.Client(), provider, statusManagers[index])
			controlID := fmt.Sprintf("concurrent-%d", index)
			message := []byte("MSH|^~\\&|A|B|C|D|20260101000000||ORM^O01|" + controlID + "|P|2.5\r")
			frame := append([]byte{mllpStart}, message...)
			frame = append(frame, mllpEnd, mllpCR)
			if _, err := clinic.Write(frame); err != nil {
				t.Error(err)
				return
			}
			if _, err := readMLLPFrame(clinic, 1024); err != nil {
				t.Error(err)
			}
		}(index)
	}
	wait.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("concurrent cloud requests=%d", len(seen))
	}
}

func TestDICOMAssociationEchoAndRepeatedStoresCreateDistinctArrivals(t *testing.T) {
	dataset := []byte{0x08, 0, 0x16, 0, 'U', 'I', 4, 0, '1', '.', '2', 0}
	received := make(chan []byte, 3)
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Content-Type") != "application/dicom" || request.Header.Get("Authorization") != "Bearer "+testCredential('A') {
			t.Fatal("DICOM headers missing")
		}
		if values := request.Header.Values("Idempotency-Key"); len(values) != 0 {
			t.Fatalf("DICOM request carried Idempotency-Key: %#v", values)
		}
		sequence := requests.Add(1)
		body, _ := io.ReadAll(request.Body)
		received <- body
		if sequence == 1 {
			<-release
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(writer, `{"status":"accepted","receiptId":"receipt-%d"}`, sequence)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.DicomURL = server.URL
	clinic, relay := net.Pipe()
	defer clinic.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveDICOM(ctx, relay, cfg, server.Client(), testProvider(t, testCredential('A')), newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")))

	storageClass := "1.2.840.10008.5.1.4.1.1.2"
	instance := "1.2.826.0.1.3680043.10.543.1"
	if err := writeDICOMPDU(clinic, 0x01, associationRequest(storageClass)); err != nil {
		t.Fatal(err)
	}
	if pduType, _, err := readDICOMPDU(clinic); err != nil || pduType != 0x02 {
		t.Fatalf("associate response type=%x error=%v", pduType, err)
	}
	if err := writeDICOMPDU(clinic, 0x04, commandPDV(1, echoRequestCommand(1))); err != nil {
		t.Fatal(err)
	}
	if pduType, body, err := readDICOMPDU(clinic); err != nil || pduType != 0x04 || dimseResponseStatus(body) != 0 {
		t.Fatalf("echo response type=%x status=%x error=%v", pduType, dimseResponseStatus(body), err)
	}
	if err := writeDICOMPDU(clinic, 0x04, commandPDV(3, storeRequestCommand(2, storageClass, instance))); err != nil {
		t.Fatal(err)
	}
	if err := writeDICOMPDU(clinic, 0x04, dataPDV(3, dataset)); err != nil {
		t.Fatal(err)
	}
	part10 := <-received
	if len(part10) <= len(dataset)+132 || string(part10[128:132]) != "DICM" || !bytes.Equal(part10[len(part10)-len(dataset):], dataset) {
		t.Fatalf("invalid Part 10 stream length=%d", len(part10))
	}
	_ = clinic.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := readDICOMPDU(clinic); err == nil {
		t.Fatal("C-STORE succeeded before cloud receipt")
	}
	_ = clinic.SetReadDeadline(time.Time{})
	close(release)
	if pduType, body, err := readDICOMPDU(clinic); err != nil || pduType != 0x04 || dimseResponseStatus(body) != 0 {
		t.Fatalf("store response type=%x status=%x error=%v", pduType, dimseResponseStatus(body), err)
	}
	// A byte-identical PACS retry of the same SOP Instance is a distinct arrival.
	if err := writeDICOMPDU(clinic, 0x04, commandPDV(3, storeRequestCommand(3, storageClass, instance))); err != nil {
		t.Fatal(err)
	}
	if err := writeDICOMPDU(clinic, 0x04, dataPDV(3, dataset)); err != nil {
		t.Fatal(err)
	}
	secondPart10 := <-received
	if !bytes.Equal(secondPart10, part10) {
		t.Fatal("byte-identical repeated C-STORE was not transmitted unchanged")
	}
	if pduType, body, err := readDICOMPDU(clinic); err != nil || pduType != 0x04 || dimseResponseStatus(body) != 0 {
		t.Fatalf("second store response type=%x status=%x error=%v", pduType, dimseResponseStatus(body), err)
	}

	// Different bytes with the same SOP Instance UID are also a new arrival.
	thirdDataset := []byte{0x08, 0, 0x18, 0, 'U', 'I', 4, 0, '1', '.', '3', 0}
	if err := writeDICOMPDU(clinic, 0x04, commandPDV(3, storeRequestCommand(4, storageClass, instance))); err != nil {
		t.Fatal(err)
	}
	if err := writeDICOMPDU(clinic, 0x04, dataPDV(3, thirdDataset)); err != nil {
		t.Fatal(err)
	}
	thirdPart10 := <-received
	if bytes.Equal(thirdPart10, part10) || !bytes.Equal(thirdPart10[len(thirdPart10)-len(thirdDataset):], thirdDataset) {
		t.Fatal("byte-different repeated SOP Instance was suppressed or changed")
	}
	if pduType, body, err := readDICOMPDU(clinic); err != nil || pduType != 0x04 || dimseResponseStatus(body) != 0 {
		t.Fatalf("third store response type=%x status=%x error=%v", pduType, dimseResponseStatus(body), err)
	}
	if requests.Load() != 3 {
		t.Fatalf("C-STORE operations produced %d HTTPS requests, want 3", requests.Load())
	}
	if err := writeDICOMPDU(clinic, 0x05, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if pduType, _, err := readDICOMPDU(clinic); err != nil || pduType != 0x06 {
		t.Fatalf("release type=%x error=%v", pduType, err)
	}
}

func TestDICOMReceiptStatusMappings(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   uint16
		abort  bool
	}{
		{"new arrival", http.StatusCreated, `{"status":"accepted","receiptId":"receipt-1"}`, 0, false},
		{"business duplicate metadata is transport neutral", http.StatusCreated, `{"status":"accepted","duplicate":true,"receiptId":"receipt-2"}`, 0, false},
		{"legacy duplicate response", http.StatusOK, `{"status":"accepted","duplicate":true,"receiptId":"receipt-old"}`, 0xC000, false},
		{"upload timeout", http.StatusRequestTimeout, `{}`, 0xA700, false},
		{"rate limited", http.StatusTooManyRequests, `{}`, 0xA700, false},
		{"storage unavailable", http.StatusServiceUnavailable, `{}`, 0xA700, false},
		{"invalid object", http.StatusUnprocessableEntity, `{}`, 0xA900, false},
		{"authentication", http.StatusUnauthorized, `{}`, 0xA700, true},
		{"malformed receipt", http.StatusCreated, `{"status":"accepted"}`, 0xC000, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.Header.Get("Idempotency-Key") != "" {
					t.Error("DICOM request carried Idempotency-Key")
				}
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			reader, writer := io.Pipe()
			result := make(chan dicomIngestResult, 1)
			status := newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json"))
			go ingestDICOM(context.Background(), server.URL, server.Client(), testProvider(t, testCredential('A')), status, reader, result)
			_, _ = writer.Write([]byte("DICM"))
			_ = writer.Close()
			got := <-result
			server.Close()
			if got.status != test.want || got.abort != test.abort || requests.Load() != 1 {
				t.Fatalf("http %d -> %#v after %d requests", test.status, got, requests.Load())
			}
			status.mu.Lock()
			authenticationAttention := status.status.AuthenticationAttention
			status.mu.Unlock()
			if authenticationAttention != (test.status == http.StatusUnauthorized || test.status == http.StatusForbidden) {
				t.Fatalf("http %d authentication attention=%t", test.status, authenticationAttention)
			}
		})
	}
}

type failingCompletionReportLedger struct{}

func (failingCompletionReportLedger) Begin(_, _, _, _ string, now time.Time) (reportDeliveryRecord, bool, error) {
	return reportDeliveryRecord{State: "pending", UpdatedAt: now}, true, nil
}

func (failingCompletionReportLedger) Complete(_, _, _, _ string, _ time.Time) error {
	return errors.New("injected completion failure")
}

func TestReportReturnRequiresRejectedStateToBeDurable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		if _, readErr := readMLLPFrame(connection, 1024*1024); readErr != nil {
			return
		}
		_, _ = connection.Write([]byte("\x0bMSA|AE|report-control-rejected\r\x1c\x0d"))
	}()
	address := listener.Addr().(*net.TCPAddr)
	cfg := defaultConfig()
	cfg.ReportHost = "127.0.0.1"
	cfg.ReportPort = address.Port
	payload := "MSH|^~\\&|TELRAD|A|CLINIC|B|20260101000000||ORU^R01|report-control-rejected|P|2.5\r"
	digest := sha256.Sum256([]byte(payload))
	response := deliverReport(context.Background(), cfg, failingCompletionReportLedger{}, "delivery-rejected", "token-rejected", payload, hex.EncodeToString(digest[:]))
	if response["type"] != "reportFail" || response["error"] != "ledger_error" || response["ackCode"] != "AE" {
		t.Fatalf("report response=%#v", response)
	}
}

func TestReportReturnDeliveryAndDurableDeduplication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	deliveries := make(chan []byte, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		frame, readErr := readMLLPFrame(connection, 1024*1024)
		if readErr != nil {
			return
		}
		deliveries <- frame
		_, _ = connection.Write([]byte("\x0bMSA|AA|report-control-1\r\x1c\x0d"))
	}()
	address := listener.Addr().(*net.TCPAddr)
	cfg := defaultConfig()
	cfg.ReportHost = "127.0.0.1"
	cfg.ReportPort = address.Port
	ledger, err := openReportDeliveryLedger(filepath.Join(t.TempDir(), "relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeReportLedgerForTest(t, ledger) })
	payload := "MSH|^~\\&|TELRAD|A|CLINIC|B|20260101000000||ORU^R01|report-control-1|P|2.5\r"
	digest := sha256.Sum256([]byte(payload))
	hash := hex.EncodeToString(digest[:])
	first := deliverReport(context.Background(), cfg, ledger, "delivery-1", "token-1", payload, hash)
	if first["type"] != "reportAck" || first["ackCode"] != "AA" {
		t.Fatalf("first report response=%#v", first)
	}
	select {
	case frame := <-deliveries:
		if !bytes.Equal(frame, append(append([]byte{mllpStart}, []byte(payload)...), mllpEnd, mllpCR)) {
			t.Fatalf("report frame=%q", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("clinic did not receive returned report")
	}
	_ = listener.Close()
	second := deliverReport(context.Background(), cfg, ledger, "delivery-1", "token-1", payload, hash)
	if second["type"] != "reportAck" || second["ackCode"] != "AA" {
		t.Fatalf("deduplicated report response=%#v", second)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestDICOMUploadUsesPipeBackpressure(t *testing.T) {
	requestStarted := make(chan *http.Request, 1)
	allowRead := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestStarted <- request
		<-allowRead
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"status":"accepted","receiptId":"receipt-backpressure"}`)),
		}, nil
	})}
	reader, writer := io.Pipe()
	result := make(chan dicomIngestResult, 1)
	go ingestDICOM(context.Background(), "https://ingest.example.test/v1/relay/ingest/dicom", client, testProvider(t, testCredential('A')), newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")), reader, result)
	request := <-requestStarted
	if request.GetBody != nil || request.Header.Get("Idempotency-Key") != "" {
		t.Fatal("DICOM stream was replayable or carried an idempotency key")
	}
	payload := bytes.Repeat([]byte{0x5a}, 256*1024)
	written := make(chan error, 1)
	go func() {
		_, err := writer.Write(payload)
		written <- err
	}()
	select {
	case err := <-written:
		t.Fatalf("pipe write completed without cloud backpressure: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowRead)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if response := <-result; response.status != 0 || response.abort {
		t.Fatalf("backpressured upload result=%#v", response)
	}
}

func TestDICOMConsumedStreamFailureIsNotRetried(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.GetBody != nil || request.Header.Get("Idempotency-Key") != "" {
			t.Error("DICOM request was replayable or carried an idempotency key")
		}
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return nil, err
		}
		return nil, errors.New("cloud response was lost after upload")
	})}
	reader, writer := io.Pipe()
	result := make(chan dicomIngestResult, 1)
	go ingestDICOM(context.Background(), "https://ingest.example.test/v1/relay/ingest/dicom", client, testProvider(t, testCredential('A')), newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")), reader, result)
	if _, err := writer.Write(bytes.Repeat([]byte{0x5a}, 128*1024)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := <-result
	if response.status != 0xA700 || response.abort || requests.Load() != 1 {
		t.Fatalf("lost response result=%#v after %d requests", response, requests.Load())
	}
}

func TestDICOMClinicDisconnectCancelsCloudUpload(t *testing.T) {
	uploadComplete := make(chan struct{}, 1)
	uploadCancelled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		uploadComplete <- struct{}{}
		<-request.Context().Done()
		uploadCancelled <- struct{}{}
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.DicomURL = server.URL
	clinic, relay := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveDICOM(ctx, relay, cfg, server.Client(), testProvider(t, testCredential('A')), newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")))
	storageClass := "1.2.840.10008.5.1.4.1.1.2"
	if err := writeDICOMPDU(clinic, 0x01, associationRequest(storageClass)); err != nil {
		t.Fatal(err)
	}
	if pduType, _, err := readDICOMPDU(clinic); err != nil || pduType != 0x02 {
		t.Fatalf("associate response type=%x error=%v", pduType, err)
	}
	if err := writeDICOMPDU(clinic, 0x04, commandPDV(3, storeRequestCommand(1, storageClass, "1.2.826.0.1.3680043.10.543.10"))); err != nil {
		t.Fatal(err)
	}
	if err := writeDICOMPDU(clinic, 0x04, dataPDV(3, []byte{8, 0, 0x16, 0, 'U', 'I', 2, 0, '1', 0})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-uploadComplete:
	case <-time.After(time.Second):
		t.Fatal("cloud did not receive the completed stream")
	}
	_ = clinic.Close()
	select {
	case <-uploadCancelled:
	case <-time.After(time.Second):
		t.Fatal("clinic disconnect did not cancel the cloud upload")
	}
}

func TestDICOMRejectsWrongCalledAEAndAbortsUnsupportedDIMSE(t *testing.T) {
	wrongClinic, wrongRelay := net.Pipe()
	go serveDICOM(context.Background(), wrongRelay, defaultConfig(), http.DefaultClient, testProvider(t, testCredential('A')), newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")))
	body := associationRequest("1.2.840.10008.5.1.4.1.1.2")
	copy(body[4:20], padAE("WRONG"))
	if err := writeDICOMPDU(wrongClinic, 0x01, body); err != nil {
		t.Fatal(err)
	}
	if pduType, _, err := readDICOMPDU(wrongClinic); err != nil || pduType != 0x03 {
		t.Fatalf("association rejection type=%x error=%v", pduType, err)
	}
	_ = wrongClinic.Close()
	_ = wrongRelay.Close()

	clinic, relay := net.Pipe()
	defer clinic.Close()
	go serveDICOM(context.Background(), relay, defaultConfig(), http.DefaultClient, testProvider(t, testCredential('A')), newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")))
	if err := writeDICOMPDU(clinic, 0x01, associationRequest("1.2.840.10008.5.1.4.1.1.2")); err != nil {
		t.Fatal(err)
	}
	if pduType, _, err := readDICOMPDU(clinic); err != nil || pduType != 0x02 {
		t.Fatalf("association response type=%x error=%v", pduType, err)
	}
	unsupported := requestCommand(1, 0x0110, verificationSOPClass, "", 0x0101)
	if err := writeDICOMPDU(clinic, 0x04, commandPDV(1, unsupported)); err != nil {
		t.Fatal(err)
	}
	if pduType, _, err := readDICOMPDU(clinic); err != nil || pduType != 0x07 {
		t.Fatalf("unsupported DIMSE abort type=%x error=%v", pduType, err)
	}
}

func TestDIMSECommandDataSetTypeSemantics(t *testing.T) {
	const (
		classUID = "1.2.840.10008.5.1.4.1.1.2"
		instance = "1.2.826.0.1.3680043.10.543.20"
	)
	tests := []struct {
		name        string
		datasetType uint16
		hasDataset  bool
	}{
		{name: "zero", datasetType: 0x0000, hasDataset: true},
		{name: "DCMTK", datasetType: 0x0001, hasDataset: true},
		{name: "ACR-NEMA compatible", datasetType: 0x0102, hasDataset: true},
		{name: "null", datasetType: 0x0101, hasDataset: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, err := parseDIMSECommand(requestCommand(1, 0x0001, classUID, instance, test.datasetType))
			if err != nil {
				t.Fatal(err)
			}
			if command.HasDataset != test.hasDataset {
				t.Fatalf("HasDataset=%t want %t", command.HasDataset, test.hasDataset)
			}
		})
	}
}

func TestPart10HeaderSupportsEveryPassThroughSyntaxDeterministically(t *testing.T) {
	for syntax := range supportedTransferSyntaxes {
		first := buildPart10Header("1.2.840.10008.5.1.4.1.1.2", "1.2.3.4", syntax)
		second := buildPart10Header("1.2.840.10008.5.1.4.1.1.2", "1.2.3.4", syntax)
		if !bytes.Equal(first, second) || len(first) < 132 || !bytes.Contains(first, []byte(syntax)) {
			t.Fatalf("invalid header for %s", syntax)
		}
	}
}

func testProvider(t *testing.T, credential string) *credentialProvider {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := commitCredential(path, credentialFile{SchemaVersion: 1, Credential: credential}); err != nil {
		t.Fatal(err)
	}
	provider, err := newCredentialProvider(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func associationRequest(storageClass string) []byte {
	body := make([]byte, 68)
	binary.BigEndian.PutUint16(body[:2], 1)
	copy(body[4:20], padAE("TELRAD"))
	copy(body[20:36], padAE("MODALITY"))
	body = append(body, associationItem(0x10, []byte("1.2.840.10008.3.1.1.1"))...)
	body = append(body, requestedContext(1, verificationSOPClass, "1.2.840.10008.1.2")...)
	body = append(body, requestedContext(3, storageClass, "1.2.840.10008.1.2.1")...)
	body = append(body, associationItem(0x50, associationItem(0x51, []byte{0, 1, 0, 0}))...)
	return body
}

func requestedContext(id byte, abstract, transfer string) []byte {
	value := []byte{id, 0, 0, 0}
	value = append(value, associationItem(0x30, []byte(abstract))...)
	value = append(value, associationItem(0x40, []byte(transfer))...)
	return associationItem(0x20, value)
}

func commandPDV(contextID byte, command []byte) []byte {
	result := make([]byte, 6+len(command))
	binary.BigEndian.PutUint32(result[:4], uint32(len(command)+2))
	result[4], result[5] = contextID, 0x03
	copy(result[6:], command)
	return result
}

func dataPDV(contextID byte, data []byte) []byte {
	result := make([]byte, 6+len(data))
	binary.BigEndian.PutUint32(result[:4], uint32(len(data)+2))
	result[4], result[5] = contextID, 0x02
	copy(result[6:], data)
	return result
}

func echoRequestCommand(messageID uint16) []byte {
	return requestCommand(messageID, 0x0030, verificationSOPClass, "", 0x0101)
}

func storeRequestCommand(messageID uint16, classUID, instance string) []byte {
	return requestCommand(messageID, 0x0001, classUID, instance, 0x0001)
}

func requestCommand(messageID, field uint16, classUID, instance string, datasetType uint16) []byte {
	var body bytes.Buffer
	writeCommandUI(&body, 0x0002, classUID)
	writeCommandUS(&body, 0x0100, field)
	writeCommandUS(&body, 0x0110, messageID)
	writeCommandUS(&body, 0x0800, datasetType)
	if instance != "" {
		writeCommandUI(&body, 0x1000, instance)
	}
	group := make([]byte, 12)
	binary.LittleEndian.PutUint32(group[4:], 4)
	binary.LittleEndian.PutUint32(group[8:], uint32(body.Len()))
	return append(group, body.Bytes()...)
}

func dimseResponseStatus(pdata []byte) uint16 {
	pdvs, err := parsePDVs(pdata)
	if err != nil || len(pdvs) != 1 {
		return 0xffff
	}
	data := pdvs[0].data
	for offset := 0; offset+8 <= len(data); {
		element := binary.LittleEndian.Uint16(data[offset+2:])
		length := int(binary.LittleEndian.Uint32(data[offset+4:]))
		offset += 8
		if offset+length > len(data) {
			return 0xffff
		}
		if element == 0x0900 && length == 2 {
			return binary.LittleEndian.Uint16(data[offset:])
		}
		offset += length
	}
	return 0xffff
}

func TestPreviewConformance(t *testing.T) {
	if os.Getenv("TELRAD_RELAY_PREVIEW_TEST") != "1" {
		t.Skip("TELRAD_RELAY_PREVIEW_TEST is not enabled")
	}
	credential := os.Getenv("TELRAD_RELAY_PREVIEW_CREDENTIAL")
	if !credentialPattern.MatchString(credential) {
		t.Skip("TELRAD_RELAY_PREVIEW_CREDENTIAL was unavailable")
	}
	request, _ := http.NewRequest(http.MethodPost, "https://ingest.dev.app.telrad.com.au/v1/relay/ingest/hl7", strings.NewReader("MSH|^~\\&|SYNTHETIC|TEST|TELRAD|TEST|20260101000000||ORM^O01|preview-conformance|P|2.5\r"))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Telrad-Protocol-Version", "1")
	request.Header.Set("Idempotency-Key", strings.Repeat("a", 32))
	request.Header.Set("Content-Type", "application/hl7-v2")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("preview returned %d", response.StatusCode)
	}
	if !mediaTypeEquals(response.Header.Get("Content-Type"), "application/hl7-v2") {
		t.Fatal("preview returned an invalid ACK content type")
	}
	ack, err := io.ReadAll(io.LimitReader(response.Body, maxCloudResponseBytes+1))
	if err != nil || len(ack) > maxCloudResponseBytes {
		t.Fatal("preview returned an invalid ACK body")
	}
	code, controlID, err := parseHL7Acknowledgement(ack)
	if err != nil || controlID != "preview-conformance" || (code != "AA" && code != "AE" && code != "AR") {
		t.Fatal("preview returned an invalid ACK")
	}
}
