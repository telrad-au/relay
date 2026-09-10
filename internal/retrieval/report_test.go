package retrieval

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestReportPermitStrictEnvelopeAndPurpose(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := ReportPermit{Version: 1, Purpose: "report-delivery", ProcessingID: "P", CompanyID: "company", ConnectorID: "relay", SourcePolicyID: "source", Examination: Examination{Accession: "ACC", Issuer: "RIS", AccessionSource: "OBR-18"}, Procedure: Procedure{Sequence: 1, SourceSetID: "1"}, HL7SHA256: strings.Repeat("0", 64), IssuedAt: "2000-01-01T00:00:00.000Z", ReportHost: "ris.example.invalid", ReportPort: 2576}
	envelope, err := SignReport(p, "clinic", key)
	if err != nil {
		t.Fatal(err)
	}
	trusted := map[string]ed25519.PublicKey{"clinic": pub}
	if got, err := VerifyReport(envelope, trusted); err != nil || got != p {
		t.Fatal("valid report permit rejected", err)
	}
	if _, _, err := Verify(envelope, trusted); err == nil {
		t.Fatal("report permit authorized retrieval")
	}
	payload, _ := json.Marshal(p)
	for name, body := range map[string]string{
		"duplicate property": strings.Replace(string(payload), `"version":1`, `"version":1,"version":1`, 1),
		"unknown property":   strings.Replace(string(payload), `"version":1`, `"version":1,"patient":"IGNORED"`, 1),
		"null field":         strings.Replace(string(payload), `"reportHost":"ris.example.invalid"`, `"reportHost":null`, 1),
		"fractional port":    strings.Replace(string(payload), `"reportPort":2576`, `"reportPort":2576.5`, 1),
		"cross purpose":      strings.Replace(string(payload), `"report-delivery"`, `"pacs-retrieval"`, 1),
		"unknown processing": strings.Replace(string(payload), `"processingId":"P"`, `"processingId":"D"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			header := strings.Split(envelope, ".")[0]
			input := header + "." + base64.RawURLEncoding.EncodeToString([]byte(body))
			signed := input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
			if _, err := VerifyReport(signed, trusted); err == nil {
				t.Fatal("signed malformed permit accepted")
			}
		})
	}
	for _, bad := range []string{"", envelope + "=", envelope + ".", strings.Repeat("A", MaxPermitBytes+1)} {
		if _, err := VerifyReport(bad, trusted); err == nil {
			t.Fatal("malformed envelope accepted")
		}
	}
	if _, err := VerifyReport(envelope, nil); err == nil {
		t.Fatal("untrusted key accepted")
	}
}
