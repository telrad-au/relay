package retrieval

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const ReportPermitType = "telrad-report-permit-v1"

type ReportPermit struct {
	ProcessingID   string      `json:"processingId"`
	Version        int         `json:"version"`
	Purpose        string      `json:"purpose"`
	ConnectorID    string      `json:"connectorId"`
	SourcePolicyID string      `json:"sourcePolicyId"`
	Examination    Examination `json:"examination"`
	Procedure      Procedure   `json:"procedure"`
	HL7SHA256      string      `json:"hl7Sha256"`
	IssuedAt       string      `json:"issuedAt"`
	ReportHost     string      `json:"reportHost"`
	ReportPort     int         `json:"reportPort"`
}

func (p ReportPermit) Validate() error {
	if p.Version != 1 || p.Purpose != "report-delivery" || (p.ProcessingID != "P" && p.ProcessingID != "T") {
		return ErrPermit
	}
	for _, value := range []string{p.ConnectorID, p.SourcePolicyID} {
		if !Opaque(value) {
			return ErrPermit
		}
	}
	for _, value := range []string{p.Examination.Accession, p.Examination.Issuer, p.Procedure.SourceSetID, p.ReportHost} {
		if !Identifier(value) {
			return ErrPermit
		}
	}
	if !AccessionSource(p.Examination.AccessionSource) || p.Procedure.Sequence < 1 || p.Procedure.Sequence > 64 || !digest.MatchString(p.HL7SHA256) || p.ReportPort < 1 || p.ReportPort > 65535 {
		return ErrPermit
	}

	t, e := time.Parse("2006-01-02T15:04:05.000Z", p.IssuedAt)
	if e != nil || t.Format("2006-01-02T15:04:05.000Z") != p.IssuedAt {
		return ErrPermit
	}
	return nil
}
func SignReport(p ReportPermit, kid string, key ed25519.PrivateKey) (string, error) {
	if p.Validate() != nil || !Opaque(kid) || len(key) != ed25519.PrivateKeySize {
		return "", ErrPermit
	}
	h, _ := json.Marshal(map[string]string{"alg": "Ed25519", "typ": ReportPermitType, "kid": kid})
	b, _ := json.Marshal(p)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input))), nil
}
func VerifyReport(envelope string, trusted map[string]ed25519.PublicKey) (ReportPermit, error) {
	var p ReportPermit
	parts := strings.Split(envelope, ".")
	if len(envelope) > MaxPermitBytes || len(parts) != 3 {
		return p, ErrPermit
	}
	h, e := DecodeBase64(parts[0], -1)
	if e != nil {
		return p, e
	}
	b, e := DecodeBase64(parts[1], -1)
	if e != nil {
		return p, e
	}
	sig, e := DecodeBase64(parts[2], 64)
	if e != nil {
		return p, e
	}
	var header struct {
		Alg   string `json:"alg"`
		Type  string `json:"typ"`
		KeyID string `json:"kid"`
	}
	if exactObject(h, []string{"alg", "typ", "kid"}) != nil || StrictJSON(h, &header) != nil || header.Alg != "Ed25519" || header.Type != ReportPermitType || !Opaque(header.KeyID) {
		return p, ErrPermit
	}
	key := trusted[header.KeyID]
	if len(key) != ed25519.PublicKeySize {
		return p, ErrTrust
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return p, ErrPermit
	}
	if exactObject(b, []string{"processingId", "version", "purpose", "connectorId", "sourcePolicyId", "examination", "procedure", "hl7Sha256", "issuedAt", "reportHost", "reportPort"}) != nil || StrictJSON(b, &p) != nil || p.Validate() != nil {
		return ReportPermit{}, ErrPermit
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(b, &fields)
	if exactObject(fields["examination"], []string{"accession", "issuer", "accessionSource"}) != nil || exactObject(fields["procedure"], []string{"sequence", "sourceSetId"}) != nil {
		return ReportPermit{}, ErrPermit
	}
	return p, nil
}
