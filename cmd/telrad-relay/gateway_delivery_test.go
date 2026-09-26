package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const deliveryTestToken = "synthetic-gateway-delivery-token-0123456789abcdef"

func writeDeliveryTestToken(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(deliveryTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return deliveryTestToken
}

func risTestAck(code string) string {
	return "MSH|^~\\&|RIS|TEST|TELRAD|TEST|20260910000000||ACK|ack|P|2.5\rMSA|" + code + "|report-1\r"
}

// fakeRIS answers each MLLP message with an ACK carrying code. When hold is
// non-nil, it waits for hold to close before answering.
func fakeRIS(t *testing.T, code string, hold <-chan struct{}) (int, *atomic.Int32, chan string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var connections atomic.Int32
	received := make(chan string, 16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				frame, err := readMLLPFrame(conn, 1<<20)
				if err != nil {
					return
				}
				select {
				case received <- string(frame[1 : len(frame)-2]):
				default:
				}
				if hold != nil {
					<-hold
				}
				_, _ = conn.Write([]byte("\x0b" + risTestAck(code) + "\x1c\r"))
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, &connections, received
}

func testDeliveryHandler(slots int) *gatewayDeliveryHandler {
	return &gatewayDeliveryHandler{tokenDigest: sha256.Sum256([]byte(deliveryTestToken)), slots: make(chan struct{}, slots), work: newWorkDrainer()}
}

func testDelivery(port int) gatewayDeliveryRequest {
	report := reportTestMessage(reportTestPayload("ACC"), "")
	return gatewayDeliveryRequest{DeliveryID: "delivery-1", Destination: &reportDestination{Host: "127.0.0.1", Port: port}, MessageControlID: report.MessageControlID, Payload: report.Payload, PayloadSHA256: report.PayloadSHA256}
}

func postDelivery(handler http.Handler, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	var data []byte
	switch value := body.(type) {
	case string:
		data = []byte(value)
	default:
		data, _ = json.Marshal(value)
	}
	request := httptest.NewRequest(http.MethodPost, gatewayDeliveryPath, bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer "+deliveryTestToken)
	request.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(request)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeDeliveryResult(t *testing.T, recorder *httptest.ResponseRecorder) gatewayDeliveryResult {
	t.Helper()
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body)
	}
	var result gatewayDeliveryResult
	if err := decodeBoundedJSON(recorder.Body, maxCloudResponseBytes, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func expectDeliveryError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if recorder.Code != status || json.Unmarshal(recorder.Body.Bytes(), &body) != nil || body.Error != code {
		t.Fatalf("status=%d body=%s, want %d %q", recorder.Code, recorder.Body, status, code)
	}
}

func TestGatewayDeliveryRoutesOnlyPostDeliveries(t *testing.T) {
	handler := testDeliveryHandler(1)
	for _, test := range []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, gatewayDeliveryPath, http.StatusMethodNotAllowed, "method_not_allowed"},
		{http.MethodPut, gatewayDeliveryPath, http.StatusMethodNotAllowed, "method_not_allowed"},
		{http.MethodPost, "/", http.StatusNotFound, "not_found"},
		{http.MethodPost, "/deliveries/delivery-1", http.StatusNotFound, "not_found"},
		{http.MethodGet, "/v1/relay/control", http.StatusNotFound, "not_found"},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		request.Header.Set("Authorization", "Bearer "+deliveryTestToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		expectDeliveryError(t, recorder, test.status, test.code)
		if test.status == http.StatusMethodNotAllowed && recorder.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("Allow = %q", recorder.Header().Get("Allow"))
		}
	}
}

func TestGatewayDeliveryRequiresTheBearerToken(t *testing.T) {
	port, connections, _ := fakeRIS(t, "AA", nil)
	handler := testDeliveryHandler(4)
	for name, mutate := range map[string]func(*http.Request){
		"missing":      func(r *http.Request) { r.Header.Del("Authorization") },
		"wrong scheme": func(r *http.Request) { r.Header.Set("Authorization", "Basic "+deliveryTestToken) },
		"wrong token": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", len(deliveryTestToken)))
		},
		"token prefix":  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+deliveryTestToken[:40]) },
		"token suffix":  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+deliveryTestToken+"x") },
		"no credential": func(r *http.Request) { r.Header.Set("Authorization", "Bearer") },
		"two headers":   func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+deliveryTestToken) },
		"padded":        func(r *http.Request) { r.Header.Set("Authorization", "Bearer  "+deliveryTestToken) },
		"bare token":    func(r *http.Request) { r.Header.Set("Authorization", deliveryTestToken) },
		"wrong and body": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer nope")
			r.Header.Set("Content-Type", "text/plain")
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := postDelivery(handler, testDelivery(port), mutate)
			expectDeliveryError(t, recorder, http.StatusUnauthorized, "unauthorized")
			if recorder.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q", recorder.Header().Get("WWW-Authenticate"))
			}
		})
	}
	if connections.Load() != 0 {
		t.Fatalf("unauthorized deliveries reached the RIS %d times", connections.Load())
	}
	if result := decodeDeliveryResult(t, postDelivery(handler, testDelivery(port), func(r *http.Request) { r.Header.Set("Authorization", "bearer "+deliveryTestToken) })); result.Outcome != "accepted" {
		t.Fatalf("case-insensitive scheme: %+v", result)
	}
}

func TestGatewayDeliveryRejectsMalformedRequestsWithoutContactingTheRIS(t *testing.T) {
	port, connections, _ := fakeRIS(t, "AA", nil)
	handler := testDeliveryHandler(4)
	valid, _ := json.Marshal(testDelivery(port))
	withField := func(name string, value any) string {
		var fields map[string]any
		_ = json.Unmarshal(valid, &fields)
		if value == nil {
			delete(fields, name)
		} else {
			fields[name] = value
		}
		data, _ := json.Marshal(fields)
		return string(data)
	}
	for name, test := range map[string]struct {
		body   string
		mutate func(*http.Request)
	}{
		"text content type":    {string(valid), func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		"missing content type": {string(valid), func(r *http.Request) { r.Header.Del("Content-Type") }},
		"not JSON":             {"MSH|^~\\&|", nil},
		"empty":                {"", nil},
		"array":                {"[" + string(valid) + "]", nil},
		"trailing value":       {string(valid) + "{}", nil},
		"unknown field":        {withField("token", "claim-1"), nil},
		"legacy authorization": {withField("authorization", "permit"), nil},
		"missing delivery ID":  {withField("deliveryId", nil), nil},
		"unsafe delivery ID":   {withField("deliveryId", "delivery 1"), nil},
		"missing destination":  {withField("destination", nil), nil},
		"null destination":     {strings.Replace(withField("destination", "x"), `"x"`, "null", 1), nil},
		"string port":          {withField("destination", map[string]any{"host": "127.0.0.1", "port": strconv.Itoa(port)}), nil},
		"destination field":    {withField("destination", map[string]any{"host": "127.0.0.1", "port": port, "ae": "RIS"}), nil},
		"missing control ID":   {withField("messageControlId", nil), nil},
		"missing payload":      {withField("payload", nil), nil},
		"missing digest":       {withField("payloadSha256", nil), nil},
		"numeric payload":      {withField("payload", 7), nil},
		"oversize body":        {withField("payload", strings.Repeat("x", maxDeliveryRequestBytes)), nil},
	} {
		t.Run(name, func(t *testing.T) {
			expectDeliveryError(t, postDelivery(handler, test.body, test.mutate), http.StatusBadRequest, "invalid_request")
		})
	}
	if connections.Load() != 0 {
		t.Fatalf("malformed deliveries reached the RIS %d times", connections.Load())
	}
	if handler.work.Active() != 0 || len(handler.slots) != 0 {
		t.Fatalf("rejected requests leaked work=%d slots=%d", handler.work.Active(), len(handler.slots))
	}
}

func TestGatewayDeliveryInvalidReportsNeverReachTheRIS(t *testing.T) {
	port, connections, _ := fakeRIS(t, "AA", nil)
	handler := testDeliveryHandler(4)
	for name, mutate := range map[string]func(*gatewayDeliveryRequest){
		"hostname destination":    func(r *gatewayDeliveryRequest) { r.Destination.Host = "localhost" },
		"IPv6 destination":        func(r *gatewayDeliveryRequest) { r.Destination.Host = "::1" },
		"mapped IPv6 destination": func(r *gatewayDeliveryRequest) { r.Destination.Host = "::ffff:127.0.0.1" },
		"non-canonical IPv4":      func(r *gatewayDeliveryRequest) { r.Destination.Host = "127.000.000.001" },
		"unspecified destination": func(r *gatewayDeliveryRequest) { r.Destination.Host = "0.0.0.0" },
		"empty host":              func(r *gatewayDeliveryRequest) { r.Destination.Host = "" },
		"zero port":               func(r *gatewayDeliveryRequest) { r.Destination.Port = 0 },
		"oversize port":           func(r *gatewayDeliveryRequest) { r.Destination.Port = 65536 },
		"order injection":         func(r *gatewayDeliveryRequest) { r.Payload = strings.Replace(r.Payload, "ORU^R01", "ORM^O01", 1) },
		"embedded message":        func(r *gatewayDeliveryRequest) { r.Payload += "MSH|^~\\&|X|X|X|X|20260101||ORU^R01|x|P|2.5\r" },
		"MLLP framing":            func(r *gatewayDeliveryRequest) { r.Payload += "\x1c\r" },
		"digest mismatch":         func(r *gatewayDeliveryRequest) { r.PayloadSHA256 = strings.Repeat("0", 64) },
		"uppercase digest":        func(r *gatewayDeliveryRequest) { r.PayloadSHA256 = strings.ToUpper(r.PayloadSHA256) },
		"control ID mismatch":     func(r *gatewayDeliveryRequest) { r.MessageControlID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			delivery := testDelivery(port)
			mutate(&delivery)
			if name != "digest mismatch" && name != "uppercase digest" && name != "control ID mismatch" {
				delivery.PayloadSHA256 = reportTestMessage(delivery.Payload, "").PayloadSHA256
			}
			result := decodeDeliveryResult(t, postDelivery(handler, delivery, nil))
			if result != (gatewayDeliveryResult{DeliveryID: "delivery-1", Outcome: "failed", Error: "invalid_report"}) {
				t.Fatalf("invalid report result: %+v", result)
			}
		})
	}
	if connections.Load() != 0 {
		t.Fatalf("invalid reports reached the RIS %d times", connections.Load())
	}
}

func TestGatewayDeliveryReturnsTheRISAcknowledgement(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	port, connections, received := fakeRIS(t, "AA", nil)
	handler := testDeliveryHandler(4)
	delivery := testDelivery(port)
	result := decodeDeliveryResult(t, postDelivery(handler, delivery, nil))
	if result != (gatewayDeliveryResult{DeliveryID: "delivery-1", Outcome: "accepted", AckCode: "AA", AckPayload: risTestAck("AA")}) {
		t.Fatalf("accepted delivery: %+v", result)
	}
	if connections.Load() != 1 || <-received != delivery.Payload {
		t.Fatalf("RIS connections = %d", connections.Load())
	}

	rejectingPort, _, _ := fakeRIS(t, "AE", nil)
	result = decodeDeliveryResult(t, postDelivery(handler, testDelivery(rejectingPort), nil))
	if result != (gatewayDeliveryResult{DeliveryID: "delivery-1", Outcome: "failed", AckCode: "AE", AckPayload: risTestAck("AE"), Error: "clinic_rejected"}) {
		t.Fatalf("rejected delivery: %+v", result)
	}

	result = decodeDeliveryResult(t, postDelivery(handler, testDelivery(freeLoopbackPort(t)), nil))
	if result != (gatewayDeliveryResult{DeliveryID: "delivery-1", Outcome: "failed", Error: "network_error"}) {
		t.Fatalf("unreachable RIS: %+v", result)
	}

	text := logs.String()
	if strings.Count(text, "gateway report delivery") != 3 || !strings.Contains(text, "deliveryId=delivery-1") || !strings.Contains(text, "destination=127.0.0.1:"+strconv.Itoa(port)) || !strings.Contains(text, "outcome=accepted") || !strings.Contains(text, "durationMs=") {
		t.Fatalf("delivery log lines: %s", text)
	}
	for _, private := range []string{"PATIENT", "Synthetic finding", "report-1", "MSA", "MSH"} {
		if strings.Contains(text, private) {
			t.Fatalf("delivery log contains %q: %s", private, text)
		}
	}
}

func TestGatewayDeliveryConcurrencyCapAndDrain(t *testing.T) {
	hold := make(chan struct{})
	port, connections, _ := fakeRIS(t, "AA", hold)
	handler := testDeliveryHandler(1)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- postDelivery(handler, testDelivery(port), nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for connections.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if connections.Load() != 1 {
		t.Fatal("first delivery did not reach the RIS")
	}

	busy := postDelivery(handler, testDelivery(port), nil)
	expectDeliveryError(t, busy, http.StatusServiceUnavailable, "busy")
	if busy.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", busy.Header().Get("Retry-After"))
	}

	// Shutdown waits for the in-flight exchange and refuses new requests.
	drained := handler.work.BeginDrain()
	refused := postDelivery(handler, testDelivery(port), nil)
	expectDeliveryError(t, refused, http.StatusServiceUnavailable, "shutting_down")
	if refused.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", refused.Header().Get("Retry-After"))
	}
	select {
	case <-drained:
		t.Fatal("drain finished while a delivery was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(hold)
	if result := decodeDeliveryResult(t, <-first); result.Outcome != "accepted" {
		t.Fatalf("in-flight delivery: %+v", result)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish after the delivery completed")
	}
	if connections.Load() != 1 {
		t.Fatalf("refused deliveries reached the RIS: %d", connections.Load())
	}
}

func TestDeliveryTokenFileRules(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "secrets")
	path := filepath.Join(directory, "delivery-token")
	writeDeliveryTestToken(t, path)
	for name, test := range map[string]struct {
		content string
		file    os.FileMode
		dir     os.FileMode
		valid   bool
	}{
		"trailing newline":    {deliveryTestToken + "\n", 0o600, 0o700, true},
		"no newline":          {deliveryTestToken, 0o600, 0o700, true},
		"CRLF":                {deliveryTestToken + "\r\n", 0o600, 0o700, true},
		"shortest":            {strings.Repeat("a", 32), 0o600, 0o700, true},
		"longest":             {strings.Repeat("a", 256), 0o600, 0o700, true},
		"too short":           {strings.Repeat("a", 31), 0o600, 0o700, false},
		"too long":            {strings.Repeat("a", 257), 0o600, 0o700, false},
		"space":               {deliveryTestToken + " x", 0o600, 0o700, false},
		"two newlines":        {deliveryTestToken + "\n\n", 0o600, 0o700, false},
		"control character":   {deliveryTestToken + "\x00", 0o600, 0o700, false},
		"non-ASCII":           {deliveryTestToken + "é", 0o600, 0o700, false},
		"group-readable file": {deliveryTestToken, 0o640, 0o700, false},
		"open directory":      {deliveryTestToken, 0o600, 0o755, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, test.file); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(directory, test.dir); err != nil {
				t.Fatal(err)
			}
			token, err := readDeliveryToken(path)
			if test.valid != (err == nil) {
				t.Fatalf("token=%q error=%v", token, err)
			}
			if test.valid && strings.ContainsAny(string(token), "\r\n") {
				t.Fatalf("token kept its line ending: %q", token)
			}
			if err != nil && strings.Contains(err.Error(), "aaaa") {
				t.Fatalf("error disclosed the token: %v", err)
			}
		})
	}
	_ = os.Chmod(directory, 0o700)
	if _, err := readDeliveryToken(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing token file was accepted")
	}
}

func selfSignedDeliveryCertificate(t *testing.T, directory string) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic gateway"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath, keyPath := filepath.Join(directory, "delivery.crt"), filepath.Join(directory, "delivery.key")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	certificate, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return certificatePath, keyPath, pool
}

func TestGatewayDeliveryListenerServesTLSWhenConfigured(t *testing.T) {
	port, _, _ := fakeRIS(t, "AA", nil)
	directory := t.TempDir()
	cfg := pairedTestConfig(directory)
	cfg.Mode, cfg.DeliveryListenAddress, cfg.DeliveryPort, cfg.DeliveryTokenPath, cfg.MaxConcurrentDeliveries = relayModeGateway, "127.0.0.1", freeLoopbackPort(t), "delivery-token", 2
	writeDeliveryTestToken(t, gatewayConfigRelative(cfg, cfg.DeliveryTokenPath))
	certificatePath, _, pool := selfSignedDeliveryCertificate(t, directory)
	cfg.DeliveryTLSCertPath, cfg.DeliveryTLSKeyPath = "delivery.crt", "delivery.key"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deliveries, err := listenGatewayDeliveries(ctx, cfg, newWorkDrainer())
	if err != nil {
		t.Fatal(err)
	}
	defer deliveries.server.Close()
	go deliveries.serve(ctx, make(chan error, 1))

	body, _ := json.Marshal(testDelivery(port))
	address := "127.0.0.1:" + strconv.Itoa(cfg.DeliveryPort) + gatewayDeliveryPath
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	request, _ := http.NewRequest(http.MethodPost, "https://"+address, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+deliveryTestToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result gatewayDeliveryResult
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&result) != nil || result.Outcome != "accepted" {
		t.Fatalf("TLS delivery: %d %+v", response.StatusCode, result)
	}
	if plain, err := http.Post("http://"+address, "application/json", bytes.NewReader(body)); err == nil {
		_ = plain.Body.Close()
		if plain.StatusCode == http.StatusOK {
			t.Fatal("TLS delivery listener accepted plain HTTP")
		}
	}

	// A certificate without its key fails before a listener is bound.
	if err := os.WriteFile(filepath.Join(directory, "delivery.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.DeliveryPort = freeLoopbackPort(t)
	if _, err := listenGatewayDeliveries(ctx, cfg, newWorkDrainer()); err == nil || strings.Contains(err.Error(), certificatePath) {
		t.Fatalf("invalid TLS key error = %v", err)
	}
}

// The platform pushes each report to the gateway. The gateway opens no
// control session, is ready without one, and returns the RIS acknowledgement.
func TestGatewayReceivesPushedReportsWithoutAControlSession(t *testing.T) {
	cfg, configPath, token, platform := startTestGateway(t)
	deadline := time.Now().Add(5 * time.Second)
	var readyErr error
	for {
		if readyErr = runtimeReady(cfg, configPath); readyErr == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if readyErr != nil {
		t.Fatalf("gateway was not ready: %v", readyErr)
	}
	status, err := readRuntimeStatus(configPath)
	if err != nil || status.ControlConnected || !status.ReportReturnAvailable || !status.IngestReady {
		t.Fatalf("runtime status = %+v error=%v", status, err)
	}

	port, connections, received := fakeRIS(t, "AA", nil)
	delivery := testDelivery(port)
	body, _ := json.Marshal(delivery)
	address := "http://" + net.JoinHostPort(cfg.DeliveryListenAddress, strconv.Itoa(cfg.DeliveryPort)) + gatewayDeliveryPath
	post := func(authorization string) *http.Response {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, address, bytes.NewReader(body))
		request.Header.Set("Authorization", authorization)
		request.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	unauthorized := post("Bearer " + strings.Repeat("z", len(token)))
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized || connections.Load() != 0 {
		t.Fatalf("unauthorized delivery status=%d RIS connections=%d", unauthorized.StatusCode, connections.Load())
	}
	response := post("Bearer " + token)
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	var result gatewayDeliveryResult
	if response.StatusCode != http.StatusOK || json.Unmarshal(data, &result) != nil {
		t.Fatalf("delivery status=%d body=%s", response.StatusCode, data)
	}
	if result != (gatewayDeliveryResult{DeliveryID: "delivery-1", Outcome: "accepted", AckCode: "AA", AckPayload: risTestAck("AA")}) {
		t.Fatalf("delivery result: %+v", result)
	}
	if connections.Load() != 1 || <-received != delivery.Payload {
		t.Fatalf("RIS connections = %d", connections.Load())
	}

	// Give a control supervisor time to reveal itself before checking.
	time.Sleep(200 * time.Millisecond)
	if requests := platform.snapshot(); len(requests) != 0 {
		t.Fatalf("gateway called the platform: %+v", requests)
	}
}
