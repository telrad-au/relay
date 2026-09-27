package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testPKI is a throwaway certificate authority standing in for Telrad's.
type testPKI struct {
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
	pool   *x509.CertPool
	serial int64
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Telrad Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testPKI{caKey: key, caCert: cert, pool: pool, serial: 1}
}

func (pki *testPKI) serverCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pki.serial++
	template := &x509.Certificate{
		SerialNumber: big.NewInt(pki.serial), Subject: pkix.Name{CommonName: "telrad-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, pki.caCert, &key.PublicKey, pki.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, pki.caCert.Raw}, PrivateKey: key}
}

// issueClient signs a CSR the way Telrad's enrolment endpoint would.
func (pki *testPKI) issueClient(t *testing.T, csrPEM string, relayID string, lifetime time.Duration) string {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatal("request is not a PEM CSR")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.CheckSignature(); err != nil {
		t.Fatal(err)
	}
	pki.serial++
	template := &x509.Certificate{
		SerialNumber: big.NewInt(pki.serial), Subject: pkix.Name{CommonName: relayID},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(lifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, pki.caCert, request.PublicKey, pki.caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pki.caCert.Raw})
	return string(leaf) + string(chain)
}

// mutualTLSListener accepts only connections presenting a certificate from the test CA.
func (pki *testPKI) mutualTLSListener(t *testing.T) net.Listener {
	t.Helper()
	config := &tls.Config{
		Certificates: []tls.Certificate{pki.serverCertificate(t)},
		ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: pki.pool, MinVersion: tls.VersionTLS12,
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func listenerPort(t *testing.T, listener net.Listener) int {
	t.Helper()
	return listener.Addr().(*net.TCPAddr).Port
}

func testConfig(t *testing.T, pki *testPKI) *config {
	t.Helper()
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.ReportHost = "127.0.0.1"
	cfg.StatusAddress = "127.0.0.1:0"
	cfg.ConnectTimeoutSeconds = 2
	cfg.TelradAckSeconds = 5
	cfg.ReceiverAckSeconds = 2
	cfg.HL7FrameSeconds = 5
	cfg.IdleTimeoutSeconds = 5
	if pki != nil {
		cfg.rootCAs = pki.pool
	}
	return cfg
}

// pairedStore writes an identity as if pairing had completed.
func pairedStore(t *testing.T, cfg *config, pki *testPKI, endpoints telradEndpoints) *identityStore {
	t.Helper()
	store, err := openIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key, csr, err := generateKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	certificate := pki.issueClient(t, csr, "relay-test", 90*24*time.Hour)
	_, leaf, err := parseCertificateChain(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.install(key, &issuedIdentity{RelayID: "relay-test", Certificate: certificate, Telrad: endpoints, leaf: leaf}); err != nil {
		t.Fatal(err)
	}
	return store
}

type testRelay struct {
	*relay
	pki       *testPKI
	dicomPort int
	hl7Port   int
	report    net.Listener
}

// newTestRelay builds a relay whose Telrad endpoints are the given fake
// listeners. Missing listeners get an unused port so dials fail fast.
func newTestRelay(t *testing.T, pki *testPKI, dicom, hl7, report net.Listener) *testRelay {
	t.Helper()
	cfg := testConfig(t, pki)
	endpoints := telradEndpoints{Host: "127.0.0.1", DicomPort: 1, HL7Port: 1, ReportPort: 1}
	if dicom != nil {
		endpoints.DicomPort = listenerPort(t, dicom)
	}
	if hl7 != nil {
		endpoints.HL7Port = listenerPort(t, hl7)
	}
	if report != nil {
		endpoints.ReportPort = listenerPort(t, report)
	}
	store := pairedStore(t, cfg, pki, endpoints)
	ledgerStore, err := openLedger(filepath.Join(cfg.DataDir, ledgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledgerStore.close() })
	status := newStatusServer(cfg, store, ledgerStore)
	return &testRelay{relay: &relay{cfg: cfg, store: store, ledger: ledgerStore, status: status}, pki: pki, report: report}
}

// serveClinicListener runs one relay handler on a loopback listener.
func serveClinicListener(t *testing.T, r *relay, handle func(context.Context, net.Conn)) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go r.serveListener(ctx, listener, 4, "test", handle)
	t.Cleanup(func() { cancel(); listener.Close() })
	return listener
}

// echoServer copies whatever it receives back to the sender, connection by connection.
func echoServer(t *testing.T, listener net.Listener) {
	t.Helper()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buffer := make([]byte, 4096)
				for {
					count, err := conn.Read(buffer)
					if count > 0 {
						if _, err := conn.Write(buffer[:count]); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
}

// hl7Responder answers each framed message with the acknowledgement respond returns.
func hl7Responder(t *testing.T, listener net.Listener, respond func(message []byte) []byte) *messageLog {
	t.Helper()
	log := &messageLog{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					frame, err := readMLLPFrame(reader, 8*1024*1024)
					if err != nil {
						return
					}
					message := unframe(frame)
					log.add(message)
					reply := respond(message)
					if reply == nil {
						continue
					}
					if _, err := conn.Write(frameMessage(reply)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return log
}

type messageLog struct {
	mu       sync.Mutex
	messages [][]byte
}

func (log *messageLog) add(message []byte) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.messages = append(log.messages, append([]byte(nil), message...))
}

func (log *messageLog) count() int {
	log.mu.Lock()
	defer log.mu.Unlock()
	return len(log.messages)
}

func (log *messageLog) last() []byte {
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.messages) == 0 {
		return nil
	}
	return log.messages[len(log.messages)-1]
}

// Synthetic messages. Identifiers are invented; no real patient data.
const (
	testOrder = "MSH|^~\\&|RIS|CLINIC|TELRAD|TELRAD|20260927101500||ORM^O01^ORM_O01|MSG0001|P|2.5.1\r" +
		"PID|1||PAT0001^^^CLINIC^MR||TEST^PATIENT||19700101|F\r" +
		"ORC|NW|PLACER0001|FILLER0001\r" +
		"OBR|1|PLACER0001|FILLER0001|CT001^CT Head||20260927101500||||||||||||ACC0001\r"
	testReport = "MSH|^~\\&|TELRAD|TELRAD|RIS|CLINIC|20260927120000||ORU^R01^ORU_R01|RPT0001|P|2.5.1\r" +
		"PID|1||PAT0001^^^CLINIC^MR||TEST^PATIENT||19700101|F\r" +
		"OBR|1|PLACER0001|FILLER0001|CT001^CT Head||||||||||||||ACC0001|||||||F\r" +
		"OBX|1|TX|REPORT^Report||Normal study.||||||F\r"
)

func ackFor(message []byte, code string) []byte {
	parsed, err := parseHL7(message)
	if err != nil {
		return nil
	}
	return []byte("MSH|^~\\&|TELRAD|TELRAD|RIS|CLINIC|20260927101501||ACK^O01^ACK|ACK" + parsed.controlID() + "|P|2.5.1\rMSA|" + code + "|" + parsed.controlID() + "\r")
}

func withControlID(message string, controlID string) string {
	parsed, _ := parseHL7([]byte(message))
	return replaceOnce(message, "|"+parsed.controlID()+"|", "|"+controlID+"|")
}

func replaceOnce(value, old, replacement string) string {
	for index := 0; index+len(old) <= len(value); index++ {
		if value[index:index+len(old)] == old {
			return value[:index] + replacement + value[index+len(old):]
		}
	}
	return value
}

func mllpExchange(t *testing.T, conn net.Conn, reader *bufio.Reader, message []byte) []byte {
	t.Helper()
	if _, err := conn.Write(frameMessage(message)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	frame, err := readMLLPFrame(reader, 8*1024*1024)
	if err != nil {
		t.Fatalf("read acknowledgement: %v", err)
	}
	return unframe(frame)
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func bufioReader(conn net.Conn) *bufio.Reader { return bufio.NewReader(conn) }
