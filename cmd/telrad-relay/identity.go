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
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	identityFileName          = "identity.json"
	enrolmentProtocolVersion  = "2"
	enrolmentRequestTimeout   = 60 * time.Second
	enrolmentMaxResponseBytes = 256 * 1024
	renewalWindow             = 30 * 24 * time.Hour
	renewalRetryInterval      = 24 * time.Hour
	renewalCheckInterval      = time.Hour
	pairingRetryDelay         = 30 * time.Second
	certificateExpiredText    = "certificate expired; pair this relay again"
	minimumPollInterval       = 3 * time.Second
	maximumPollInterval       = 60 * time.Second
)

type certificatePool = *x509.CertPool

// telradEndpoints is where and how Relay reaches Telrad's data ports.
// CACertificate is the Telrad Relay CA, the only trust anchor for those ports.
type telradEndpoints struct {
	Host          string `json:"host"`
	DicomPort     int    `json:"dicomPort"`
	HL7Port       int    `json:"hl7Port"`
	ReportPort    int    `json:"reportPort"`
	CACertificate string `json:"caCertificate,omitempty"`
}

// identityFile is the one file that holds everything pairing produced. Writing
// it atomically means a renewal can never leave a key without its certificate.
type identityFile struct {
	SchemaVersion int             `json:"schemaVersion"`
	RelayID       string          `json:"relayId"`
	PrivateKeyPEM string          `json:"privateKey"`
	Certificate   string          `json:"certificate"`
	NotAfter      time.Time       `json:"notAfter"`
	Telrad        telradEndpoints `json:"telrad"`
}

type identityStore struct {
	mu        sync.RWMutex
	path      string
	current   *identityFile
	tlsCert   *tls.Certificate
	leaf      *x509.Certificate
	telradCAs certificatePool
	changedCh chan struct{}
}

func openIdentity(cfg *config) (*identityStore, error) {
	store := &identityStore{path: cfg.dataPath(identityFileName), changedCh: make(chan struct{}, 1)}
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var file identityFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, errors.New("stored identity is invalid")
	}
	if file.Telrad.CACertificate == "" {
		// Identities from before the Telrad Relay CA was pinned cannot verify
		// the data ports. Pairing again replaces the file.
		slog.Warn("stored identity has no Telrad Relay CA certificate; pairing again")
		return store, nil
	}
	if err := store.adopt(&file); err != nil {
		return nil, fmt.Errorf("stored identity is invalid: %w", err)
	}
	return store, nil
}

func (store *identityStore) adopt(file *identityFile) error {
	if file.SchemaVersion != 1 || !validOpaqueID(file.RelayID) {
		return errors.New("unsupported identity file")
	}
	if err := validateEndpoints(file.Telrad); err != nil {
		return err
	}
	key, err := parsePrivateKeyPEM(file.PrivateKeyPEM)
	if err != nil {
		return err
	}
	chain, leaf, err := parseCertificateChain(file.Certificate, key)
	if err != nil {
		return err
	}
	pool, err := parseCAPool(file.Telrad.CACertificate)
	if err != nil {
		return err
	}
	certificate := &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}
	store.mu.Lock()
	store.current, store.tlsCert, store.leaf, store.telradCAs = file, certificate, leaf, pool
	store.mu.Unlock()
	return nil
}

// paired reports whether Relay holds a certificate it can still use. An
// expired identity stays loaded for status but needs pairing again.
func (store *identityStore) paired() bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.current != nil && time.Now().Before(store.leaf.NotAfter)
}

// expired reports whether the stored certificate can no longer be used.
func (store *identityStore) expired() bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.current != nil && !time.Now().Before(store.leaf.NotAfter)
}

func (store *identityStore) relayID() string {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.current == nil {
		return ""
	}
	return store.current.RelayID
}

func (store *identityStore) endpoints() telradEndpoints {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.current == nil {
		return telradEndpoints{}
	}
	return store.current.Telrad
}

func (store *identityStore) notAfter() time.Time {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.leaf == nil {
		return time.Time{}
	}
	return store.leaf.NotAfter
}

// changed is signalled once after every successful renewal.
func (store *identityStore) changed() <-chan struct{} { return store.changedCh }

func (store *identityStore) clientCertificate() (*tls.Certificate, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.tlsCert == nil {
		return nil, errors.New("relay is not paired")
	}
	return store.tlsCert, nil
}

// clientTLS returns the configuration used for every connection to Telrad's
// data ports. Telrad is verified against the pinned Telrad Relay CA only, never
// the operating system's roots. The certificate is resolved per handshake so a
// renewal applies to the next dial.
func (store *identityStore) clientTLS(serverName string) *tls.Config {
	store.mu.RLock()
	pool := store.telradCAs
	store.mu.RUnlock()
	if pool == nil {
		pool = x509.NewCertPool() // unpaired: trust nothing rather than the system roots
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
		RootCAs:    pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return store.clientCertificate()
		},
	}
}

func (store *identityStore) install(key *ecdsa.PrivateKey, issued *issuedIdentity) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	file := &identityFile{
		SchemaVersion: 1, RelayID: issued.RelayID,
		PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		Certificate:   issued.Certificate, NotAfter: issued.leaf.NotAfter, Telrad: issued.Telrad,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(store.path, append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := store.adopt(file); err != nil {
		return err
	}
	select {
	case store.changedCh <- struct{}{}:
	default:
	}
	return nil
}

func generateKeyAndCSR() (*ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	// The subject is empty on purpose: Telrad assigns the identity.
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{}}, key)
	if err != nil {
		return nil, "", err
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

func parsePrivateKeyPEM(value string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, errors.New("private key is not EC PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// parseCertificateChain accepts a PEM bundle whose first certificate is the
// leaf for key. Any following certificates are sent as the chain. Expiry is
// the caller's decision: a stored identity loads expired so status can say so.
func parseCertificateChain(value string, key *ecdsa.PrivateKey) ([][]byte, *x509.Certificate, error) {
	var chain [][]byte
	rest := []byte(value)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, nil, errors.New("certificate bundle contains a non-certificate block")
		}
		chain = append(chain, block.Bytes)
	}
	if len(chain) == 0 {
		return nil, nil, errors.New("certificate bundle is empty")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, nil, errors.New("leaf certificate is invalid")
	}
	public, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !public.Equal(key.Public()) {
		return nil, nil, errors.New("certificate does not match the relay key")
	}
	return chain, leaf, nil
}

// parseCAPool accepts a PEM bundle of one or more CA certificates: the Telrad
// Relay CA that Relay pins for the data ports.
func parseCAPool(value string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	count := 0
	rest := []byte(value)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, errors.New("telrad CA bundle contains a non-certificate block")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !certificate.BasicConstraintsValid || !certificate.IsCA {
			return nil, errors.New("telrad CA bundle contains a certificate that is not a CA")
		}
		pool.AddCert(certificate)
		count++
	}
	if count == 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("telrad CA certificate is missing or invalid")
	}
	return pool, nil
}

func validOpaqueID(value string) bool {
	if len(value) < 1 || len(value) > 200 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func validateEndpoints(endpoints telradEndpoints) error {
	host := strings.TrimSpace(endpoints.Host)
	if host == "" || strings.ContainsAny(host, " /:@?#") {
		return errors.New("telrad host is invalid")
	}
	for name, port := range map[string]int{"dicomPort": endpoints.DicomPort, "hl7Port": endpoints.HL7Port, "reportPort": endpoints.ReportPort} {
		if port < 1 || port > 65535 {
			return fmt.Errorf("telrad %s is invalid", name)
		}
	}
	return nil
}

func (endpoints telradEndpoints) address(port int) string {
	return net.JoinHostPort(endpoints.Host, strconv.Itoa(port))
}

// Enrolment protocol.

type issuedIdentity struct {
	RelayID     string          `json:"relayId"`
	Certificate string          `json:"certificate"`
	NotAfter    time.Time       `json:"notAfter"`
	Telrad      telradEndpoints `json:"telrad"`

	leaf *x509.Certificate
}

type pendingEnrolment struct {
	EnrolmentID     string    `json:"enrolmentId"`
	VerificationURL string    `json:"verificationUrl"`
	PollSeconds     int       `json:"pollSeconds"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

var errEnrolmentPending = errors.New("enrolment pending")
var errEnrolmentExpired = errors.New("pairing link expired")
var errEnrolmentDenied = errors.New("pairing was denied")
var errEnrolmentRedirect = errors.New("enrolment endpoint redirected; redirects are not followed")

type enrolmentClient struct {
	cfg    *config
	client *http.Client
}

// newEnrolmentClient verifies Telrad's enrolment endpoint with the operating
// system's roots and presents no client certificate; renewal is signed instead.
func newEnrolmentClient(cfg *config) *enrolmentClient {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: cfg.rootCAs}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       tlsConfig,
		DialContext:           (&net.Dialer{Timeout: time.Duration(cfg.ConnectTimeoutSeconds) * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableKeepAlives:     true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   enrolmentRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errEnrolmentRedirect
		},
	}
	return &enrolmentClient{cfg: cfg, client: client}
}

func enrolmentBody(csr string, extra map[string]string) ([]byte, error) {
	hostname, _ := os.Hostname()
	body := map[string]string{"csr": csr, "agentVersion": version, "platform": relayPlatform(), "hostname": hostname}
	for key, value := range extra {
		body[key] = value
	}
	return json.Marshal(body)
}

func (client *enrolmentClient) do(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Telrad-Relay-Protocol", enrolmentProtocolVersion)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.client.Do(request)
	if err != nil {
		if errors.Is(err, errEnrolmentRedirect) {
			return nil, errEnrolmentRedirect
		}
		return nil, fmt.Errorf("enrolment request failed: %w", safeNetworkError(err))
	}
	return response, nil
}

func decodeJSONResponse(response *http.Response, target any) error {
	defer response.Body.Close()
	if !mediaTypeEquals(response.Header.Get("Content-Type"), "application/json") {
		return errors.New("enrolment response is not JSON")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, enrolmentMaxResponseBytes+1))
	if err != nil || len(data) > enrolmentMaxResponseBytes {
		return errors.New("enrolment response is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("enrolment response is invalid")
	}
	return nil
}

// issuedFrom validates an issued identity for key. A response without
// caCertificate keeps currentCA, which is empty when pairing, so pairing must
// supply one and a renewal may replace it.
func (client *enrolmentClient) issuedFrom(response *http.Response, key *ecdsa.PrivateKey, currentCA string) (*issuedIdentity, error) {
	var issued issuedIdentity
	if err := decodeJSONResponse(response, &issued); err != nil {
		return nil, err
	}
	if issued.Telrad.CACertificate == "" {
		issued.Telrad.CACertificate = currentCA
	}
	if _, err := parseCAPool(issued.Telrad.CACertificate); err != nil {
		return nil, fmt.Errorf("enrolment response: %w", err)
	}
	if !validOpaqueID(issued.RelayID) {
		return nil, errors.New("enrolment response has an invalid relay identifier")
	}
	if err := validateEndpoints(issued.Telrad); err != nil {
		return nil, fmt.Errorf("enrolment response: %w", err)
	}
	_, leaf, err := parseCertificateChain(issued.Certificate, key)
	if err != nil {
		return nil, fmt.Errorf("enrolment response: %w", err)
	}
	if !leaf.NotAfter.After(time.Now()) {
		return nil, errors.New("enrolment response: certificate has expired")
	}
	issued.leaf = leaf
	return &issued, nil
}

// begin starts interactive pairing. Telrad answers with the link a person opens.
func (client *enrolmentClient) begin(ctx context.Context, csr string) (*pendingEnrolment, error) {
	body, err := enrolmentBody(csr, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.do(ctx, http.MethodPost, client.cfg.EnrolmentURL, body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusCreated {
		response.Body.Close()
		return nil, fmt.Errorf("enrolment failed: http_%d", response.StatusCode)
	}
	var pending pendingEnrolment
	if err := decodeJSONResponse(response, &pending); err != nil {
		return nil, err
	}
	if !validOpaqueID(pending.EnrolmentID) || !pending.ExpiresAt.After(time.Now()) {
		return nil, errors.New("enrolment response is invalid")
	}
	link, err := parseURL(pending.VerificationURL)
	if err != nil || link.Scheme != "https" || link.Host == "" || link.User != nil {
		return nil, errors.New("enrolment response has an invalid verification link")
	}
	return &pending, nil
}

// poll asks whether the person has approved. It returns errEnrolmentPending
// while waiting, errEnrolmentExpired when the link lapsed and
// errEnrolmentDenied when the person refused it.
func (client *enrolmentClient) poll(ctx context.Context, pending *pendingEnrolment, key *ecdsa.PrivateKey) (*issuedIdentity, error) {
	response, err := client.do(ctx, http.MethodGet, strings.TrimRight(client.cfg.EnrolmentURL, "/")+"/"+pending.EnrolmentID, nil)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusOK:
		return client.issuedFrom(response, key, "")
	case http.StatusAccepted:
		response.Body.Close()
		return nil, errEnrolmentPending
	case http.StatusGone, http.StatusNotFound:
		response.Body.Close()
		return nil, errEnrolmentExpired
	case http.StatusForbidden:
		response.Body.Close()
		return nil, errEnrolmentDenied
	default:
		response.Body.Close()
		return nil, fmt.Errorf("enrolment poll failed: http_%d", response.StatusCode)
	}
}

// withToken pairs a container with a single-use token that already names the company.
func (client *enrolmentClient) withToken(ctx context.Context, csr string, token string, key *ecdsa.PrivateKey) (*issuedIdentity, error) {
	if len(token) < 16 || len(token) > 200 {
		return nil, errors.New("pairing token has an invalid length")
	}
	body, err := enrolmentBody(csr, map[string]string{"pairingToken": token})
	if err != nil {
		return nil, err
	}
	response, err := client.do(ctx, http.MethodPost, client.cfg.EnrolmentURL, body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		if response.StatusCode == http.StatusForbidden {
			return nil, errors.New("pairing token was refused")
		}
		return nil, fmt.Errorf("enrolment failed: http_%d", response.StatusCode)
	}
	return client.issuedFrom(response, key, "")
}

// renewalRequest is the body of POST {enrolmentUrl}/renew. The current key
// signs the new CSR, so the request needs no client certificate.
type renewalRequest struct {
	CSR          string `json:"csr"`
	AgentVersion string `json:"agentVersion"`
	Certificate  string `json:"certificate"`
	SignedAt     string `json:"signedAt"`
	Signature    string `json:"signature"`
}

// renewalSigningInput is what the current key signs: csr || "\n" || signedAt.
func renewalSigningInput(csr, signedAt string) []byte {
	digest := sha256.Sum256([]byte(csr + "\n" + signedAt))
	return digest[:]
}

// signRenewal builds a renewal request for csr, signed with the current key as
// a raw P-256 signature (r || s, 32 bytes each) and carrying the current leaf.
func signRenewal(csr string, current *tls.Certificate, now time.Time) (*renewalRequest, error) {
	key, ok := current.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || current.Leaf == nil {
		return nil, errors.New("current identity cannot sign a renewal")
	}
	signedAt := now.UTC().Format(time.RFC3339)
	r, s, err := ecdsa.Sign(rand.Reader, key, renewalSigningInput(csr, signedAt))
	if err != nil {
		return nil, err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return &renewalRequest{
		CSR: csr, AgentVersion: version, SignedAt: signedAt,
		Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: current.Leaf.Raw})),
		Signature:   base64.StdEncoding.EncodeToString(signature),
	}, nil
}

// renew obtains a certificate for a fresh key with a request signed by the
// current one. currentCA is kept unless the response carries a new CA.
func (client *enrolmentClient) renew(ctx context.Context, request *renewalRequest, key *ecdsa.PrivateKey, currentCA string) (*issuedIdentity, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	response, err := client.do(ctx, http.MethodPost, strings.TrimRight(client.cfg.EnrolmentURL, "/")+"/renew", body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("renewal failed: http_%d", response.StatusCode)
	}
	return client.issuedFrom(response, key, currentCA)
}

// pairInteractively runs the link flow until a certificate is issued or ctx ends.
// An expired or denied link is replaced by a new one; a network failure is retried.
func pairInteractively(ctx context.Context, cfg *config, store *identityStore, status *statusServer) error {
	client := newEnrolmentClient(cfg)
	// problem survives a replacement link so status says why a new one appeared.
	problem := ""
	for ctx.Err() == nil {
		key, csr, err := generateKeyAndCSR()
		if err != nil {
			return err
		}
		pending, err := client.begin(ctx, csr)
		if err != nil {
			status.setPairing("", err.Error())
			if !sleepContext(ctx, pairingRetryDelay) {
				return ctx.Err()
			}
			continue
		}
		status.setPairing(pending.VerificationURL, problem)
		interval := time.Duration(pending.PollSeconds) * time.Second
		if interval < minimumPollInterval {
			interval = minimumPollInterval
		}
		if interval > maximumPollInterval {
			interval = maximumPollInterval
		}
		for ctx.Err() == nil && time.Now().Before(pending.ExpiresAt) {
			if !sleepContext(ctx, interval) {
				return ctx.Err()
			}
			issued, err := client.poll(ctx, pending, key)
			if errors.Is(err, errEnrolmentPending) {
				continue
			}
			if errors.Is(err, errEnrolmentExpired) || errors.Is(err, errEnrolmentDenied) {
				problem = err.Error() + "; a new link has been issued"
				break
			}
			if err != nil {
				status.setPairing(pending.VerificationURL, err.Error())
				continue
			}
			if err := store.install(key, issued); err != nil {
				return fmt.Errorf("store identity: %w", err)
			}
			status.setPairing("", "")
			return nil
		}
	}
	return ctx.Err()
}

func pairWithToken(ctx context.Context, cfg *config, store *identityStore, token string) error {
	key, csr, err := generateKeyAndCSR()
	if err != nil {
		return err
	}
	issued, err := newEnrolmentClient(cfg).withToken(ctx, csr, token, key)
	if err != nil {
		return err
	}
	return store.install(key, issued)
}

func renewIdentity(ctx context.Context, cfg *config, store *identityStore) error {
	current, err := store.clientCertificate()
	if err != nil {
		return err
	}
	key, csr, err := generateKeyAndCSR()
	if err != nil {
		return err
	}
	request, err := signRenewal(csr, current, time.Now())
	if err != nil {
		return err
	}
	issued, err := newEnrolmentClient(cfg).renew(ctx, request, key, store.endpoints().CACertificate)
	if err != nil {
		return err
	}
	return store.install(key, issued)
}

// maintainIdentity renews the certificate from 30 days before expiry, retrying
// daily after a failure. An expired certificate cannot renew; that is reported.
func maintainIdentity(ctx context.Context, cfg *config, store *identityStore, status *statusServer) {
	var lastAttempt time.Time
	for {
		now := time.Now()
		notAfter := store.notAfter()
		if notAfter.Sub(now) <= renewalWindow && (lastAttempt.IsZero() || now.Sub(lastAttempt) >= renewalRetryInterval) {
			lastAttempt = now
			if !now.Before(notAfter) {
				status.setRenewalError(certificateExpiredText)
			} else if err := renewIdentity(ctx, cfg, store); err != nil {
				status.setRenewalError(err.Error())
			} else {
				status.setRenewalError("")
				lastAttempt = time.Time{}
			}
		}
		if !sleepContext(ctx, renewalCheckInterval) {
			return
		}
	}
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func relayPlatform() string {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if distribution != "" && distribution != "native" {
		return distribution + "/" + platform
	}
	return platform
}

// safeNetworkError strips addresses and other detail that could identify the
// clinic network from an error before it reaches logs or status output.
func safeNetworkError(err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errors.New("timeout")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errors.New("dns_failure")
	}
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return errors.New("tls_verification_failed")
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("network_failure")
}
