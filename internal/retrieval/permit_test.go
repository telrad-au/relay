package retrieval

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixturePermit() Permit {
	return Permit{Version: 2, Purpose: "pacs-retrieval", Kind: "accession", CompanyID: "company", ConnectorID: "connector", PACSID: "pacs", SourcePolicyID: "policy", Examination: Examination{"ACC", "CLINIC", "OBR-18"}, Procedure: Procedure{1, "1"}, HL7SHA256: strings.Repeat("a", 64), IssuedAt: "2000-01-01T00:00:00.000Z"}
}
func TestSharedSigningVectors(t *testing.T) {
	b, e := os.ReadFile("testdata/vectors.json")
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		PublicKey string
		Vectors   []struct {
			Name     string
			Valid    bool
			Envelope string
		}
	}
	if e := json.Unmarshal(b, &f); e != nil {
		t.Fatal(e)
	}
	pub, e := DecodeBase64(f.PublicKey, 32)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range f.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			_, _, e := Verify(v.Envelope, map[string]ed25519.PublicKey{"fixture-key": pub})
			if (e == nil) != v.Valid {
				t.Fatalf("valid=%v error=%v", v.Valid, e)
			}
		})
	}
}
func signedRaw(h, p string, k ed25519.PrivateKey) string {
	input := base64.RawURLEncoding.EncodeToString([]byte(h)) + "." + base64.RawURLEncoding.EncodeToString([]byte(p))
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, []byte(input)))
}
func TestStrictSignedPermitNegatives(t *testing.T) {
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(fixturePermit())
	payload := string(raw)
	header := `{"alg":"Ed25519","typ":"telrad-pacs-permit-v2","kid":"key"}`
	cases := map[string]string{
		"duplicate":           strings.Replace(payload, `"version":2`, `"version":2,"version":2`, 1),
		"escaped duplicate":   strings.Replace(payload, `"version":2`, `"version":2,"\u0076ersion":2`, 1),
		"case alias":          strings.Replace(payload, `"version":2`, `"Version":2`, 1),
		"unknown":             strings.Replace(payload, `"version":2`, `"version":2,"expiry":"never"`, 1),
		"missing":             strings.Replace(payload, `"kind":"accession",`, "", 1),
		"null":                strings.Replace(payload, `"kind":"accession"`, `"kind":null`, 1),
		"nested alias":        strings.Replace(payload, `"accessionSource"`, `"AccessionSource"`, 1),
		"nested extra":        strings.Replace(payload, `"accessionSource":"OBR-18"`, `"accessionSource":"OBR-18","study":"1.2"`, 1),
		"wildcard":            strings.Replace(payload, `"ACC"`, `"PAT*ENT"`, 1),
		"surrogate":           strings.Replace(payload, `"ACC"`, `"\ud800"`, 1),
		"low surrogate":       strings.Replace(payload, `"ACC"`, `"\udc00"`, 1),
		"invalid UTF8":        strings.Replace(payload, "ACC", string([]byte{0xff}), 1),
		"bad date":            strings.Replace(payload, "2000-01-01", "2000-02-30", 1),
		"legacy scope":        strings.Replace(payload, `"kind":"accession"`, `"kind":"study"`, 1),
		"fractional sequence": strings.Replace(payload, `"sequence":1`, `"sequence":1.5`, 1),
		"overflow sequence":   strings.Replace(payload, `"sequence":1`, `"sequence":65`, 1),
		"null metadata":       strings.Replace(payload, `"version":2`, `"version":2,"clinicCompleted":null`, 1),
		"depth":               `{"x":` + strings.Repeat("[", 13) + "0" + strings.Repeat("]", 13) + "}",
		"trailing":            payload + "{}",
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, e := Verify(signedRaw(header, p, key), map[string]ed25519.PublicKey{"key": pub}); e == nil {
				t.Fatal("accepted malformed signed scope")
			}
		})
	}
	for name, h := range map[string]string{"algorithm": strings.Replace(header, "Ed25519", "EdDSA", 1), "type": strings.Replace(header, "v2", "v1", 1), "extra": strings.Replace(header, `"kid":"key"`, `"kid":"key","b64":true`, 1), "duplicate": strings.Replace(header, `"kid":"key"`, `"kid":"key","k\u0069d":"key"`, 1)} {
		t.Run(name, func(t *testing.T) {
			if _, _, e := Verify(signedRaw(h, payload, key), map[string]ed25519.PublicKey{"key": pub}); e == nil {
				t.Fatal("accepted header")
			}
		})
	}
	good := signedRaw(header, payload, key)
	for _, bad := range []string{good + "=", good + ".x", strings.Repeat("a", MaxPermitBytes+1), "." + good} {
		if _, _, e := Verify(bad, map[string]ed25519.PublicKey{"key": pub}); e == nil {
			t.Fatal("accepted bad envelope")
		}
	}
}
func TestPermanentReplayAndTrustRetirement(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	envelope, e := Sign(fixturePermit(), "key", key)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		p, _, e := Verify(envelope, map[string]ed25519.PublicKey{"key": pub})
		if e != nil || p.IssuedAt != "2000-01-01T00:00:00.000Z" {
			t.Fatal("permanent replay rejected")
		}
	}
	if _, _, e := Verify(envelope, nil); e != ErrTrust {
		t.Fatal("retired key authorized work")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, e := Verify(envelope, map[string]ed25519.PublicKey{"key": other}); e == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestStrictJSONUnicodeAndDepth(t *testing.T) {
	for _, s := range []string{`{"a":"\ud83d\ude00"}`, `{"a":"é"}`, `{"a":"\\ud800"}`} {
		var v any
		if StrictJSON([]byte(s), &v) != nil {
			t.Fatal("valid Unicode rejected")
		}
	}
	for _, s := range []string{`{"a":0,"\u0061":1}`, `{"a":"\ud800x"}`, `{"a":"\udc00"}`, "[" + strings.Repeat("[", 13) + strings.Repeat("]", 14)} {
		var v any
		if StrictJSON([]byte(s), &v) == nil {
			t.Fatal("malformed JSON accepted")
		}
	}
}

func TestAccessionPermitWithoutPatient(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	p := fixturePermit()
	envelope, err := Sign(p, "key", key)
	if err != nil {
		t.Fatal(err)
	}
	verified, _, err := Verify(envelope, map[string]ed25519.PublicKey{"key": pub})
	if err != nil || verified.Examination.Accession != p.Examination.Accession {
		t.Fatal("accession permit failed", err)
	}
	raw, _ := json.Marshal(p)
	header := `{"alg":"Ed25519","typ":"telrad-pacs-permit-v2","kid":"key"}`
	for _, patient := range []string{`null`, `{}`, `{"id":"PATIENT"}`} {
		bad := strings.Replace(string(raw), `"version":2`, `"version":2,"patient":`+patient, 1)
		if _, _, err := Verify(signedRaw(header, bad, key), map[string]ed25519.PublicKey{"key": pub}); err == nil {
			t.Fatal("accepted unsupported patient predicate")
		}
	}
}
