package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

// Runtime-generated keys only. Shared by polling tests and report benchmarks.
func reportTestSigner(t testing.TB, cfg *config) func(string) string {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg.configPath = ""
	if cfg.RelayID == "" {
		cfg.RelayID = "synthetic-relay"
	}
	cfg.ReportAuthorization = &reportAuthorizationConfig{CompanyID: "synthetic-company", ConnectorID: cfg.RelayID, SigningKeyID: permitKeyID(pub), TrustedPublicKeys: []string{base64.RawURLEncoding.EncodeToString(pub)}, Policies: []reportSourcePolicy{{ID: "synthetic-source", SourceAddresses: []string{"127.0.0.1"}, SendingApplication: "SYNTHETIC", SendingFacility: "CLINIC", AccessionSource: "OBR-18", AccessionIssuer: "CLINIC"}}}
	return func(payload string) string {
		segments, err := referralSegments([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		accession := ""
		for _, s := range segments {
			if s[0] == "OBR" {
				accession = field(s, 18)
			}
		}
		grant := retrieval.ReportPermit{Version: 1, Purpose: "report-delivery", ProcessingID: "P", CompanyID: cfg.ReportAuthorization.CompanyID, ConnectorID: cfg.RelayID, SourcePolicyID: "synthetic-source", Examination: retrieval.Examination{Accession: accession, Issuer: "CLINIC", AccessionSource: "OBR-18"}, Procedure: retrieval.Procedure{Sequence: 1, SourceSetID: "1"}, HL7SHA256: strings.Repeat("0", 64), IssuedAt: "2000-01-01T00:00:00.000Z", ReportHost: cfg.ReportHost, ReportPort: cfg.ReportPort}
		envelope, err := retrieval.SignReport(grant, permitKeyID(pub), key)
		if err != nil {
			t.Fatal(err)
		}
		return envelope
	}
}
func reportTestPayload(accession string) string {
	obr := make([]string, 19)
	obr[0] = "OBR"
	obr[1] = "1"
	obr[4] = "CT^Synthetic"
	obr[18] = accession
	return "MSH|^~\\&|TELRAD|TEST|RIS|TEST|20260910000000||ORU^R01|report-1|P|2.5\rPID|1||PATIENT\r" + strings.Join(obr, "|") + "\rOBX|1|TX|REPORT||Synthetic finding\r"
}
func reportTestMessage(payload, authorization string) reportMessage {
	sum := sha256.Sum256([]byte(payload))
	return reportMessage{Type: "report", DeliveryID: "delivery-1", Token: "claim-1", MessageControlID: "report-1", Payload: payload, PayloadSHA256: hex.EncodeToString(sum[:]), Authorization: authorization, ClaimExpiresAt: time.Now().Add(time.Minute)}
}
func TestReportAuthorizationBlocksCloudForgeriesBeforeMLLP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
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
			conn.Close()
		}
	}()
	cfg := retrievalTestConfig(t)
	cfg.ReportHost = "127.0.0.1"
	cfg.ReportPort = listener.Addr().(*net.TCPAddr).Port
	saveRetrievalTestConfig(t, cfg)
	permits, err := signReportAuthorizations(cfg, &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}, retrievalTestHL7("NW", 1))
	if err != nil {
		t.Fatal(err)
	}
	payload := reportTestPayload("ACC")
	good := reportTestMessage(payload, permits[0])
	if result := deliverReport(context.Background(), cfg, good); result.Outcome != "accepted" {
		t.Fatalf("valid permit: %+v", result)
	}
	if connections.Load() != 1 {
		t.Fatal("expected one RIS delivery")
	}
	for name, mutate := range map[string]func(*reportMessage){
		"missing": func(r *reportMessage) { r.Authorization = "" },
		"forged signature": func(r *reportMessage) {
			parts := strings.Split(r.Authorization, ".")
			parts[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 64))
			r.Authorization = strings.Join(parts, ".")
		},
		"different accession":   func(r *reportMessage) { r.Payload = reportTestPayload("UNORDERED") },
		"multiple orders":       func(r *reportMessage) { r.Payload += "OBR|2|||||||||||||||||UNORDERED\r" },
		"alternate identifier":  func(r *reportMessage) { r.Payload = strings.Replace(r.Payload, "OBR|1|||", "OBR|1|UNORDERED||", 1) },
		"cloud order injection": func(r *reportMessage) { r.Payload = strings.Replace(r.Payload, "ORU^R01", "ORM^O01", 1) },
		"test report":           func(r *reportMessage) { r.Payload = strings.Replace(r.Payload, "|P|2.5", "|T|2.5", 1) },
		"MLLP injection":        func(r *reportMessage) { r.Payload += "\x1c\r\x0b" + payload },
		"retrieval purpose":     func(r *reportMessage) { r.Authorization = testSignedReferral(t, cfg) },
	} {
		t.Run(name, func(t *testing.T) {
			r := good
			mutate(&r)
			sum := sha256.Sum256([]byte(r.Payload))
			r.PayloadSHA256 = hex.EncodeToString(sum[:])
			if result := deliverReport(context.Background(), cfg, r); result.Outcome != "failed" || result.Error != "invalid_report" {
				t.Fatalf("forgery accepted: %+v", result)
			}
		})
	}
	keys, _ := cfg.Retrieval.trusted()
	original, err := retrieval.VerifyReport(good.Authorization, keys)
	if err != nil {
		t.Fatal(err)
	}
	key, err := readPermitSigningKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroBytes(key)
	for name, change := range map[string]func(*retrieval.ReportPermit){
		"company":     func(p *retrieval.ReportPermit) { p.CompanyID = "other-company" },
		"connector":   func(p *retrieval.ReportPermit) { p.ConnectorID = "other-relay" },
		"namespace":   func(p *retrieval.ReportPermit) { p.Examination.Issuer = "OTHER" },
		"policy":      func(p *retrieval.ReportPermit) { p.SourcePolicyID = "other-policy" },
		"destination": func(p *retrieval.ReportPermit) { p.ReportHost = "other-ris.example.invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			p := original
			change(&p)
			r := good
			r.Authorization, err = retrieval.SignReport(p, cfg.Retrieval.SigningKeyID, key)
			if err != nil {
				t.Fatal(err)
			}
			if result := deliverReport(context.Background(), cfg, r); result.Outcome != "failed" {
				t.Fatal("out-of-scope signed permit accepted")
			}
		})
	}
	testPermits, err := signReportAuthorizations(cfg, &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}, []byte(strings.Replace(string(retrievalTestHL7("NW", 1)), "|P|2.5", "|T|2.5", 1)))
	if err != nil {
		t.Fatal(err)
	}
	testReport := good
	testReport.Authorization = testPermits[0]
	if authorizeReport(cfg, testReport) == nil {
		t.Fatal("TEST order authorized live report")
	}
	cfg.ReportPort++
	saveRetrievalTestConfig(t, cfg)
	if authorizeReport(cfg, good) == nil {
		t.Fatal("changed destination accepted old permit")
	}
	cfg.ReportPort--
	cfg.Retrieval.TrustedPublicKeys = []string{base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
	cfg.Retrieval.SigningKeyID = permitKeyID(make([]byte, 32))
	saveRetrievalTestConfig(t, cfg)
	if authorizeReport(cfg, good) == nil {
		t.Fatal("retired clinic key accepted")
	}
	if connections.Load() != 1 {
		t.Fatal("forbidden report reached RIS")
	}
}
func TestReportAuthorizationPushSourcesAndCancellations(t *testing.T) {
	cfg := retrievalTestConfig(t)
	auth, err := reportSigningConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReportAuthorization = auth
	cfg.Retrieval = nil
	saveRetrievalTestConfig(t, cfg)
	peer := &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}
	for _, control := range []string{"NW", "XO", "CA", "DC"} {
		grants, err := signReportAuthorizations(cfg, peer, retrievalTestHL7(control, 2))
		if err != nil {
			t.Fatal(err)
		}
		expected := 2
		if control == "CA" || control == "DC" {
			expected = 0
		}
		if len(grants) != expected {
			t.Fatal("incorrect permit count")
		}
	}
	if _, err := signReportAuthorizations(cfg, &net.TCPAddr{IP: net.ParseIP("192.0.2.1")}, retrievalTestHL7("NW", 1)); err == nil {
		t.Fatal("unapproved sender signed")
	}
	if _, err := signReportAuthorizations(cfg, peer, []byte(strings.Replace(string(retrievalTestHL7("NW", 1)), "SYNTHETIC", "OTHER", 1))); err == nil {
		t.Fatal("unapproved application signed")
	}
}
