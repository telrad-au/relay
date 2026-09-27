package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEnrolment is Telrad's enrolment endpoint: link flow, token flow and renewal.
type fakeEnrolment struct {
	pki       *testPKI
	server    *httptest.Server
	mu        sync.Mutex
	approved  map[string]bool
	denied    map[string]bool
	nextID    int
	csrs      map[string]string
	token     string
	renewals  int
	endpoints telradEndpoints
	// renewEndpoints, when set, is what a renewal returns instead of endpoints.
	renewEndpoints *telradEndpoints
	lifetime       time.Duration
	redirectTo     string
}

func newFakeEnrolment(t *testing.T, pki *testPKI) *fakeEnrolment {
	t.Helper()
	fake := &fakeEnrolment{pki: pki, approved: map[string]bool{}, denied: map[string]bool{}, csrs: map[string]string{}, token: "token-0123456789abcdef", lifetime: 90 * 24 * time.Hour,
		endpoints: telradEndpoints{Host: "ingest.example.invalid", DicomPort: 2762, HL7Port: 2575, ReportPort: 2580, CACertificate: pki.caPEM()}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/relay/enrolments", fake.handleEnrol)
	mux.HandleFunc("/v1/relay/enrolments/renew", fake.handleRenew)
	mux.HandleFunc("/v1/relay/enrolments/", fake.handlePoll)
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	fake.server = httptest.NewUnstartedServer(mux)
	fake.server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate(t)}, MinVersion: tls.VersionTLS12}
	fake.server.StartTLS()
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeEnrolment) url() string { return fake.server.URL + "/v1/relay/enrolments" }

func (fake *fakeEnrolment) issue(w http.ResponseWriter, csr string, relayID string) {
	fake.issueWith(w, csr, relayID, fake.endpoints)
}

func (fake *fakeEnrolment) issueWith(w http.ResponseWriter, csr string, relayID string, endpoints telradEndpoints) {
	certificate := fake.pki.issueClient(&testing.T{}, csr, relayID, fake.lifetime)
	writeJSON(w, http.StatusOK, issuedIdentity{RelayID: relayID, Certificate: certificate, NotAfter: time.Now().Add(fake.lifetime), Telrad: endpoints})
}

func (fake *fakeEnrolment) handleEnrol(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Telrad-Relay-Protocol") != "2" || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["csr"] == "" || body["agentVersion"] == "" || body["platform"] == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if fake.redirectTo != "" {
		http.Redirect(w, r, fake.redirectTo, http.StatusFound)
		return
	}
	if token, ok := body["pairingToken"]; ok {
		if token != fake.token {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fake.issue(w, body["csr"], "relay-token")
		return
	}
	fake.mu.Lock()
	fake.nextID++
	id := "enrol-" + strings.Repeat("a", 8) + strconv.Itoa(fake.nextID)
	fake.csrs[id] = body["csr"]
	fake.mu.Unlock()
	writeJSON(w, http.StatusCreated, pendingEnrolment{EnrolmentID: id, VerificationURL: "https://app.example.invalid/relays/approve/" + id, PollSeconds: 1, ExpiresAt: time.Now().Add(10 * time.Minute)})
}

func (fake *fakeEnrolment) handlePoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/relay/enrolments/")
	fake.mu.Lock()
	csr, known := fake.csrs[id]
	approved := fake.approved[id]
	denied := fake.denied[id]
	fake.mu.Unlock()
	switch {
	case !known:
		w.WriteHeader(http.StatusGone)
	case denied:
		w.WriteHeader(http.StatusForbidden)
	case !approved:
		w.WriteHeader(http.StatusAccepted)
	default:
		fake.issue(w, csr, "relay-link")
	}
}

// handleRenew authenticates a renewal the way Telrad does: the certificate
// must chain to the Relay CA and still be valid, and its key must have signed
// csr || "\n" || signedAt within the last few minutes.
func (fake *fakeEnrolment) handleRenew(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Telrad-Relay-Protocol") != "2" || r.Method != http.MethodPost || len(r.TLS.PeerCertificates) != 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var body renewalRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.CSR == "" || body.AgentVersion == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	relayID, ok := fake.verifyRenewal(&body)
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	fake.mu.Lock()
	fake.renewals++
	endpoints := fake.renewEndpoints
	fake.mu.Unlock()
	if endpoints == nil {
		endpoints = &fake.endpoints
	}
	fake.issueWith(w, body.CSR, relayID, *endpoints)
}

func (fake *fakeEnrolment) verifyRenewal(body *renewalRequest) (string, bool) {
	block, _ := pem.Decode([]byte(body.Certificate))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", false
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: fake.pki.pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", false
	}
	signedAt, err := time.Parse(time.RFC3339, body.SignedAt)
	if err != nil || time.Since(signedAt).Abs() > 5*time.Minute {
		return "", false
	}
	signature, err := base64.StdEncoding.DecodeString(body.Signature)
	public, isECDSA := leaf.PublicKey.(*ecdsa.PublicKey)
	if err != nil || len(signature) != 64 || !isECDSA {
		return "", false
	}
	r, s := new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])
	digest := sha256.Sum256([]byte(body.CSR + "\n" + body.SignedAt))
	if !ecdsa.Verify(public, digest[:], r, s) {
		return "", false
	}
	return leaf.Subject.CommonName, true
}

func (fake *fakeEnrolment) approveAll() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for id := range fake.csrs {
		fake.approved[id] = true
	}
}

func TestPairWithTokenAndReload(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	store, err := openIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if store.paired() {
		t.Fatal("fresh store is paired")
	}
	if err := pairWithToken(context.Background(), cfg, store, "wrong-token-0123456789"); err == nil {
		t.Fatal("wrong token accepted")
	}
	if err := pairWithToken(context.Background(), cfg, store, fake.token); err != nil {
		t.Fatal(err)
	}
	if !store.paired() || store.relayID() != "relay-token" || store.endpoints() != fake.endpoints {
		t.Fatalf("store=%+v", store.current)
	}
	assertPrivateFileMode(t, filepath.Join(cfg.DataDir, identityFileName))
	reloaded, err := openIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.paired() || reloaded.relayID() != "relay-token" {
		t.Fatal("identity did not reload")
	}
	certificate, _ := reloaded.clientCertificate()
	if certificate.Leaf.Subject.CommonName != "relay-token" || len(certificate.Certificate) != 2 {
		t.Fatal("client certificate chain wrong")
	}
}

func TestPairInteractivelyPublishesLinkThenInstalls(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	store, _ := openIdentity(cfg)
	status := newStatusServer(cfg, store, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pairInteractively(ctx, cfg, store, status) }()
	waitFor(t, func() bool { return status.snapshot().PairingLink != "" })
	if link := status.snapshot().PairingLink; !strings.HasPrefix(link, "https://app.example.invalid/relays/approve/") {
		t.Fatalf("link=%q", link)
	}
	if status.snapshot().State != "pairing" {
		t.Fatalf("state=%q", status.snapshot().State)
	}
	fake.approveAll()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	report := status.snapshot()
	if !report.Paired || report.PairingLink != "" || store.relayID() != "relay-link" {
		t.Fatalf("report=%+v", report)
	}
}

func TestRenewIdentityUsesCurrentCertificateAndSignalsChange(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	store, _ := openIdentity(cfg)
	if err := renewIdentity(context.Background(), cfg, store); err == nil {
		t.Fatal("renewal without a certificate succeeded")
	}
	if err := pairWithToken(context.Background(), cfg, store, fake.token); err != nil {
		t.Fatal(err)
	}
	before, _ := store.clientCertificate()
	select {
	case <-store.changed():
	default:
		t.Fatal("pairing did not signal a change")
	}
	if err := renewIdentity(context.Background(), cfg, store); err != nil {
		t.Fatal(err)
	}
	after, _ := store.clientCertificate()
	if fake.renewals != 1 || after.Leaf.SerialNumber.Cmp(before.Leaf.SerialNumber) == 0 || after.Leaf.Subject.CommonName != "relay-token" {
		t.Fatal("renewal did not replace the certificate")
	}
	if after.PrivateKey == before.PrivateKey {
		t.Fatal("renewal reused the private key")
	}
	select {
	case <-store.changed():
	default:
		t.Fatal("renewal did not signal a change")
	}
}

func TestMaintainIdentityRenewsInsideWindow(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	fake.lifetime = 10 * 24 * time.Hour
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	store, _ := openIdentity(cfg)
	if err := pairWithToken(context.Background(), cfg, store, fake.token); err != nil {
		t.Fatal(err)
	}
	fake.lifetime = 90 * 24 * time.Hour
	status := newStatusServer(cfg, store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); maintainIdentity(ctx, cfg, store, status) }()
	waitFor(t, func() bool { return time.Until(store.notAfter()) > 80*24*time.Hour })
	cancel()
	<-done
	if fake.renewals != 1 {
		t.Fatalf("renewals=%d", fake.renewals)
	}
	if remaining := time.Until(store.notAfter()); remaining < 80*24*time.Hour {
		t.Fatalf("certificate not renewed: %v remaining", remaining)
	}
	if status.snapshot().RenewalError != "" {
		t.Fatal(status.snapshot().RenewalError)
	}
}

func TestEnrolmentRefusesRedirectsAndBadResponses(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	fake.redirectTo = fake.server.URL + "/elsewhere"
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	store, _ := openIdentity(cfg)
	if err := pairWithToken(context.Background(), cfg, store, fake.token); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect followed: %v", err)
	}
	fake.redirectTo = ""
	// A certificate for a different key must be refused.
	otherKey, otherCSR, _ := generateKeyAndCSR()
	_ = otherKey
	certificate := pki.issueClient(t, otherCSR, "relay-x", time.Hour)
	key, _, _ := generateKeyAndCSR()
	if _, _, err := parseCertificateChain(certificate, key); err == nil {
		t.Fatal("certificate for another key accepted")
	}
	if err := validateEndpoints(telradEndpoints{Host: "bad host", DicomPort: 1, HL7Port: 1, ReportPort: 1}); err == nil {
		t.Fatal("host with a space accepted")
	}
	if err := validateEndpoints(telradEndpoints{Host: "ok.example", DicomPort: 0, HL7Port: 1, ReportPort: 1}); err == nil {
		t.Fatal("zero port accepted")
	}
}

func TestOpenIdentityRejectsCorruptFile(t *testing.T) {
	cfg := testConfig(t, nil)
	if err := os.WriteFile(filepath.Join(cfg.DataDir, identityFileName), []byte(`{"schemaVersion":1,"relayId":"x","privateKey":"nope","telrad":{"caCertificate":"nope"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openIdentity(cfg); err == nil {
		t.Fatal("corrupt identity accepted")
	}
}

func TestPollDistinguishesDeniedFromExpired(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	client := newEnrolmentClient(cfg)
	key, csr, _ := generateKeyAndCSR()
	pending, err := client.begin(context.Background(), csr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.poll(context.Background(), pending, key); !errors.Is(err, errEnrolmentPending) {
		t.Fatalf("pending: %v", err)
	}
	fake.mu.Lock()
	fake.denied[pending.EnrolmentID] = true
	fake.mu.Unlock()
	if _, err := client.poll(context.Background(), pending, key); !errors.Is(err, errEnrolmentDenied) {
		t.Fatalf("denied: %v", err)
	}
	if _, err := client.poll(context.Background(), &pendingEnrolment{EnrolmentID: "enrol-unknown"}, key); !errors.Is(err, errEnrolmentExpired) {
		t.Fatalf("expired: %v", err)
	}
	if errEnrolmentDenied.Error() == errEnrolmentExpired.Error() {
		t.Fatal("status cannot distinguish denied from expired")
	}
}

// installExpiredIdentity stores an identity whose certificate has already expired.
func installExpiredIdentity(t *testing.T, cfg *config, pki *testPKI) {
	t.Helper()
	store, err := openIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key, csr, _ := generateKeyAndCSR()
	certificate := pki.issueClient(t, csr, "relay-old", -30*time.Second)
	_, leaf, err := parseCertificateChain(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := telradEndpoints{Host: "127.0.0.1", DicomPort: 1, HL7Port: 1, ReportPort: 1, CACertificate: pki.caPEM()}
	if err := store.install(key, &issuedIdentity{RelayID: "relay-old", Certificate: certificate, Telrad: endpoints, leaf: leaf}); err != nil {
		t.Fatal(err)
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	return listenerPort(t, probe)
}

func TestExpiredIdentityLoadsAndIsReportedAsNeedingPairing(t *testing.T) {
	pki := newTestPKI(t)
	cfg := testConfig(t, pki)
	installExpiredIdentity(t, cfg, pki)
	store, err := openIdentity(cfg)
	if err != nil {
		t.Fatalf("expired identity rejected: %v", err)
	}
	if store.paired() || !store.expired() {
		t.Fatal("expired identity treated as paired")
	}
	report := newStatusServer(cfg, store, nil).snapshot()
	if report.Paired || report.State != "pairing" || report.CertificateNotAfter == nil || !report.CertificateNotAfter.Before(time.Now()) || report.RenewalError != certificateExpiredText {
		t.Fatalf("report=%+v", report)
	}
}

// A native relay with an expired certificate pairs again interactively and
// replaces the stored identity.
func TestRunRelayRepairsExpiredIdentityInteractively(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	cfg.ListenAddress = "127.0.0.1"
	cfg.StatusAddress = net.JoinHostPort("127.0.0.1", strconv.Itoa(freeLoopbackPort(t)))
	cfg.DicomPort, cfg.HL7Port = freeLoopbackPort(t), freeLoopbackPort(t)
	installExpiredIdentity(t, cfg, pki)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runRelay(ctx, cfg) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("service did not stop")
		}
	}()
	waitFor(t, func() bool {
		report, err := fetchStatus(cfg.StatusAddress)
		return err == nil && report.PairingLink != "" && report.RenewalError == certificateExpiredText && !report.Paired
	})
	fake.approveAll()
	deadline := time.Now().Add(15 * time.Second)
	for !func() bool { report, err := fetchStatus(cfg.StatusAddress); return err == nil && report.Paired }() {
		if time.Now().After(deadline) {
			t.Fatal("relay did not pair again")
		}
		time.Sleep(50 * time.Millisecond)
	}
	reloaded, err := openIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.paired() || reloaded.relayID() != "relay-link" {
		t.Fatalf("stored identity not replaced: %q", reloaded.relayID())
	}
}

func TestRunRelayInContainerWithExpiredIdentityRequiresToken(t *testing.T) {
	previous := distribution
	distribution = "docker"
	t.Cleanup(func() { distribution = previous })
	pki := newTestPKI(t)
	cfg := testConfig(t, pki)
	installExpiredIdentity(t, cfg, pki)
	t.Setenv(pairingTokenVariable, "")
	if err := runRelay(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), pairingTokenVariable) {
		t.Fatalf("err=%v", err)
	}
}

// Renewal is a plain HTTPS POST signed by the current key. Telrad refuses a
// body whose signed fields were altered, a signature by another key and a
// stale signing time.
func TestRenewalIsSignedByCurrentKeyAndTamperingIsRefused(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	store, _ := openIdentity(cfg)
	if err := pairWithToken(context.Background(), cfg, store, fake.token); err != nil {
		t.Fatal(err)
	}
	current, _ := store.clientCertificate()
	client := newEnrolmentClient(cfg)
	caPEM := store.endpoints().CACertificate
	signed := func(t *testing.T) (*renewalRequest, *ecdsa.PrivateKey) {
		t.Helper()
		key, csr, _ := generateKeyAndCSR()
		request, err := signRenewal(csr, current, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return request, key
	}
	request, _ := signed(t)
	signature, _ := base64.StdEncoding.DecodeString(request.Signature)
	if len(signature) != 64 || request.Certificate == "" || strings.Count(request.Certificate, "BEGIN CERTIFICATE") != 1 || request.AgentVersion != version {
		t.Fatalf("renewal request malformed: %d-byte signature", len(signature))
	}
	if _, err := time.Parse(time.RFC3339, request.SignedAt); err != nil || !strings.HasSuffix(request.SignedAt, "Z") {
		t.Fatalf("signedAt=%q", request.SignedAt)
	}

	_, otherCSR, _ := generateKeyAndCSR()
	otherKey, _, _ := generateKeyAndCSR()
	tampered := map[string]func(*renewalRequest){
		"csr replaced":     func(r *renewalRequest) { r.CSR = otherCSR },
		"signedAt changed": func(r *renewalRequest) { r.SignedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339) },
		"signature by other": func(r *renewalRequest) {
			*r = *mustSign(t, r.CSR, &tls.Certificate{PrivateKey: otherKey, Leaf: current.Leaf}, time.Now())
		},
		"stale signature": func(r *renewalRequest) { *r = *mustSign(t, r.CSR, current, time.Now().Add(-time.Hour)) },
		"no certificate":  func(r *renewalRequest) { r.Certificate = "" },
	}
	for name, tamper := range tampered {
		request, key := signed(t)
		tamper(request)
		if _, err := client.renew(context.Background(), request, key, caPEM); err == nil || !strings.Contains(err.Error(), "http_403") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	if fake.renewals != 0 {
		t.Fatalf("renewals=%d after tampered requests", fake.renewals)
	}
	request, key := signed(t)
	issued, err := client.renew(context.Background(), request, key, caPEM)
	if err != nil || issued.RelayID != "relay-token" {
		t.Fatalf("valid renewal: %v", err)
	}
}

func mustSign(t *testing.T, csr string, current *tls.Certificate, at time.Time) *renewalRequest {
	t.Helper()
	request, err := signRenewal(csr, current, at)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// Pairing must deliver the Telrad Relay CA; a renewal may rotate it and
// otherwise keeps the stored one.
func TestTelradCACertificateIsRequiredAndRotatedByRenewal(t *testing.T) {
	pki := newTestPKI(t)
	fake := newFakeEnrolment(t, pki)
	cfg := testConfig(t, pki)
	cfg.EnrolmentURL = fake.url()
	store, _ := openIdentity(cfg)
	leaf := pki.serverCertificate(t)
	_, request, _ := generateKeyAndCSR()
	for name, value := range map[string]string{
		"missing":  "",
		"garbage":  "not a certificate",
		"not a CA": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]})),
		"request":  request,
	} {
		fake.endpoints.CACertificate = value
		if err := pairWithToken(context.Background(), cfg, store, fake.token); err == nil || store.paired() {
			t.Fatalf("%s CA accepted", name)
		}
	}
	fake.endpoints.CACertificate = pki.caPEM()
	if err := pairWithToken(context.Background(), cfg, store, fake.token); err != nil {
		t.Fatal(err)
	}

	withoutCA := fake.endpoints
	withoutCA.CACertificate = ""
	fake.renewEndpoints = &withoutCA
	if err := renewIdentity(context.Background(), cfg, store); err != nil {
		t.Fatal(err)
	}
	if store.endpoints().CACertificate != pki.caPEM() {
		t.Fatal("renewal without caCertificate dropped the stored CA")
	}

	rotated := fake.endpoints
	rotated.CACertificate = newTestPKI(t).caPEM() + pki.caPEM()
	fake.renewEndpoints = &rotated
	if err := renewIdentity(context.Background(), cfg, store); err != nil {
		t.Fatal(err)
	}
	reloaded, err := openIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.endpoints().CACertificate != rotated.CACertificate {
		t.Fatal("renewal did not replace the stored CA")
	}
}

// An identity written before the CA was pinned cannot verify the data ports,
// so it is treated as unpaired rather than trusted or rejected.
func TestOpenIdentityWithoutTelradCAIsUnpaired(t *testing.T) {
	pki := newTestPKI(t)
	cfg := testConfig(t, pki)
	pairedStore(t, cfg, pki, telradEndpoints{Host: "127.0.0.1", DicomPort: 1, HL7Port: 1, ReportPort: 1})
	path := filepath.Join(cfg.DataDir, identityFileName)
	data, _ := os.ReadFile(path)
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	delete(file["telrad"].(map[string]any), "caCertificate")
	data, _ = json.Marshal(file)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if store.paired() || store.expired() || store.relayID() != "" {
		t.Fatal("identity without a Telrad CA treated as paired")
	}
}
