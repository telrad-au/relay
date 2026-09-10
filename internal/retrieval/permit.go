// Package retrieval implements the clinic-controlled retrieval v2 trust boundary.
// It has no filesystem, scheduler, or cloud-provided endpoint configuration.
package retrieval

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const PermitType = "telrad-pacs-permit-v2"
const MaxPermitBytes = 16 * 1024

var ErrPermit = errors.New("invalid_permit")
var ErrTrust = errors.New("untrusted_key")
var ErrPolicy = errors.New("local_policy_rejected")
var opaque = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var uid = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))+$`)

func Opaque(s string) bool { return opaque.MatchString(s) }
func UID(s string) bool    { return len(s) <= 64 && uid.MatchString(s) }
func Identifier(s string) bool {
	if len(s) == 0 || len(s) > 200 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 || c == '*' || c == '?' || c == '\\' {
			return false
		}
	}
	return true
}

type Examination struct {
	Accession       string `json:"accession"`
	Issuer          string `json:"issuer"`
	AccessionSource string `json:"accessionSource"`
}
type Procedure struct {
	Sequence    int    `json:"sequence"`
	SourceSetID string `json:"sourceSetId"`
}
type Permit struct {
	Version         int         `json:"version"`
	Purpose         string      `json:"purpose"`
	Kind            string      `json:"kind"`
	CompanyID       string      `json:"companyId"`
	ConnectorID     string      `json:"connectorId"`
	PACSID          string      `json:"pacsId"`
	SourcePolicyID  string      `json:"sourcePolicyId"`
	Examination     Examination `json:"examination"`
	Procedure       Procedure   `json:"procedure"`
	HL7SHA256       string      `json:"hl7Sha256"`
	IssuedAt        string      `json:"issuedAt"`
	ClinicCompleted *bool       `json:"clinicCompleted,omitempty"`
}

func AccessionSource(s string) bool {
	switch s {
	case "OBR-18", "OBR-3", "ORC-3", "OBR-2", "ORC-2":
		return true
	}
	return false
}
func (p Permit) Validate() error {
	if p.Version != 2 || p.Purpose != "pacs-retrieval" || p.Kind != "accession" {
		return ErrPermit
	}
	for _, s := range []string{p.CompanyID, p.ConnectorID, p.PACSID, p.SourcePolicyID} {
		if !Opaque(s) {
			return ErrPermit
		}
	}
	for _, s := range []string{p.Examination.Accession, p.Examination.Issuer, p.Procedure.SourceSetID} {
		if !Identifier(s) {
			return ErrPermit
		}
	}
	if !AccessionSource(p.Examination.AccessionSource) || p.Procedure.Sequence < 1 || p.Procedure.Sequence > 64 || !digest.MatchString(p.HL7SHA256) {
		return ErrPermit
	}
	const layout = "2006-01-02T15:04:05.000Z"
	t, err := time.Parse(layout, p.IssuedAt)
	if err != nil || t.Format(layout) != p.IssuedAt {
		return ErrPermit
	}
	// issuedAt is signed metadata, never an expiry or replay restriction.
	return nil
}
func DecodeBase64(s string, size int) ([]byte, error) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	if e != nil || len(b) == 0 || base64.RawURLEncoding.EncodeToString(b) != s || (size >= 0 && len(b) != size) {
		return nil, ErrPermit
	}
	return b, nil
}

// StrictJSON rejects duplicates after escape decoding, invalid UTF-8/surrogates,
// excess nesting and trailing values before encoding/json can normalize them.
func StrictJSON(b []byte, out any) error {
	if !utf8.Valid(b) || !validJSONStrings(b) {
		return ErrPermit
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if _, err := strictValue(d, 0); err != nil {
		return ErrPermit
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrPermit
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrPermit
	}
	return nil
}
func strictValue(d *json.Decoder, depth int) (any, error) {
	if depth > 12 {
		return nil, ErrPermit
	}
	t, e := d.Token()
	if e != nil {
		return nil, ErrPermit
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				s, ok := k.(string)
				if e != nil || !ok || keys[s] || s == "__proto__" || s == "constructor" {
					return nil, ErrPermit
				}
				keys[s] = true
				if _, e = strictValue(d, depth+1); e != nil {
					return nil, e
				}
			}
			if t, e = d.Token(); e != nil || t != json.Delim('}') {
				return nil, ErrPermit
			}
		case '[':
			for d.More() {
				if _, e = strictValue(d, depth+1); e != nil {
					return nil, e
				}
			}
			if t, e = d.Token(); e != nil || t != json.Delim(']') {
				return nil, ErrPermit
			}
		default:
			return nil, ErrPermit
		}
	}
	return t, nil
}
func validJSONStrings(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		i++
		for ; i < len(b) && b[i] != '"'; i++ {
			if b[i] != '\\' {
				continue
			}
			i++
			if i >= len(b) {
				return false
			}
			if b[i] != 'u' {
				continue
			}
			if i+4 >= len(b) {
				return false
			}
			n, e := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
			if e != nil {
				return false
			}
			i += 4
			if n >= 0xdc00 && n <= 0xdfff {
				return false
			}
			if n >= 0xd800 && n <= 0xdbff {
				if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
					return false
				}
				low, e := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
				if e != nil || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
	}
	return true
}
func exactObject(b []byte, required []string, optional ...string) error {
	var m map[string]json.RawMessage
	if StrictJSON(b, &m) != nil || m == nil {
		return ErrPermit
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return ErrPermit
		}
	}
	for k, v := range m {
		allowed := false
		for _, s := range append(required, optional...) {
			if k == s {
				allowed = true
			}
		}
		if !allowed || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return ErrPermit
		}
	}
	return nil
}
func Verify(envelope string, trusted map[string]ed25519.PublicKey) (Permit, string, error) {
	var p Permit
	if len(envelope) > MaxPermitBytes {
		return p, "", ErrPermit
	}
	parts := strings.Split(envelope, ".")
	if len(parts) != 3 {
		return p, "", ErrPermit
	}
	h, e := DecodeBase64(parts[0], -1)
	if e != nil {
		return p, "", e
	}
	b, e := DecodeBase64(parts[1], -1)
	if e != nil {
		return p, "", e
	}
	sig, e := DecodeBase64(parts[2], 64)
	if e != nil {
		return p, "", e
	}
	var header struct {
		Alg   string `json:"alg"`
		Type  string `json:"typ"`
		KeyID string `json:"kid"`
	}
	if exactObject(h, []string{"alg", "typ", "kid"}) != nil || StrictJSON(h, &header) != nil || header.Alg != "Ed25519" || header.Type != PermitType || !Opaque(header.KeyID) {
		return p, "", ErrPermit
	}
	key := trusted[header.KeyID]
	if len(key) != ed25519.PublicKeySize {
		return p, header.KeyID, ErrTrust
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return p, header.KeyID, ErrPermit
	}
	if exactObject(b, []string{"version", "purpose", "kind", "companyId", "connectorId", "pacsId", "sourcePolicyId", "examination", "procedure", "hl7Sha256", "issuedAt"}, "clinicCompleted") != nil {
		return p, header.KeyID, ErrPermit
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(b, &fields)
	for name, keys := range map[string][]string{"examination": {"accession", "issuer", "accessionSource"}, "procedure": {"sequence", "sourceSetId"}} {
		if exactObject(fields[name], keys) != nil {
			return p, header.KeyID, ErrPermit
		}
	}
	if StrictJSON(b, &p) != nil || p.Validate() != nil {
		return Permit{}, header.KeyID, ErrPermit
	}
	return p, header.KeyID, nil
}
func Sign(p Permit, kid string, key ed25519.PrivateKey) (string, error) {
	if p.Validate() != nil || !Opaque(kid) || len(key) != ed25519.PrivateKeySize {
		return "", ErrPermit
	}
	h, _ := json.Marshal(map[string]string{"alg": "Ed25519", "typ": PermitType, "kid": kid})
	b, _ := json.Marshal(p)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input))), nil
}
