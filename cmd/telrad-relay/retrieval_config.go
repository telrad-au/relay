package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/telrad-au/relay/internal/retrieval"
)

// These values are local clinic approval, never populated from control responses.
type retrievalConfig struct {
	Enabled           bool             `json:"enabled"`
	CompanyID         string           `json:"companyId"`
	ConnectorID       string           `json:"connectorId"`
	SigningKeyID      string           `json:"signingKeyId"`
	TrustedPublicKeys []string         `json:"trustedPublicKeys"`
	PACS              []retrievalPACS  `json:"pacs"`
	Policies          []referralPolicy `json:"policies"`
}
type retrievalPACS struct {
	ID                    string `json:"id"`
	DICOMwebURL           string `json:"dicomwebUrl"`
	PatientIssuer         string `json:"patientIssuer"`
	AccessionIssuer       string `json:"accessionIssuer"`
	MaxStudyBytes         int64  `json:"maxStudyBytes"`
	MaxInstanceBytes      int64  `json:"maxInstanceBytes"`
	MaxInstances          int    `json:"maxInstances"`
	RequestTimeoutSeconds int    `json:"requestTimeoutSeconds"`
	// This first adapter has no completion API. Other PACS profiles need a
	// qualified adapter; this is not an operator-selectable completion policy.
	Adapter string `json:"adapter"`
}
type referralPolicy struct {
	ID                        string   `json:"id"`
	PACSID                    string   `json:"pacsId"`
	SourceAddresses           []string `json:"sourceAddresses"`
	SendingApplication        string   `json:"sendingApplication"`
	SendingFacility           string   `json:"sendingFacility"`
	OrderControls             []string `json:"orderControls"`
	AccessionSource           string   `json:"accessionSource"`
	AllowMissingPatientIssuer bool     `json:"allowMissingPatientIssuer"`
}

func permitKeyID(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return "ed25519-" + hex.EncodeToString(sum[:])
}
func (r *retrievalConfig) trusted() (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	for _, s := range r.TrustedPublicKeys {
		b, e := retrieval.DecodeBase64(s, 32)
		if e != nil {
			return nil, retrieval.ErrTrust
		}
		id := permitKeyID(b)
		if _, ok := keys[id]; ok {
			return nil, retrieval.ErrTrust
		}
		keys[id] = b
	}
	return keys, nil
}
func (r *retrievalConfig) pacs(id string) (retrievalPACS, bool) {
	for _, p := range r.PACS {
		if p.ID == id {
			return p, true
		}
	}
	return retrievalPACS{}, false
}
func (r *retrievalConfig) policy(id string) (referralPolicy, bool) {
	for _, p := range r.Policies {
		if p.ID == id {
			return p, true
		}
	}
	return referralPolicy{}, false
}
func validateRetrievalConfig(cfg *config) error {
	r := cfg.Retrieval
	if r == nil {
		return nil
	}
	if !retrieval.Opaque(r.CompanyID) || !retrieval.Opaque(r.ConnectorID) || r.ConnectorID != cfg.RelayID || len(r.TrustedPublicKeys) < 1 || len(r.TrustedPublicKeys) > 64 || len(r.PACS) < 1 || len(r.PACS) > 64 || len(r.Policies) < 1 || len(r.Policies) > 64 {
		return retrieval.ErrPolicy
	}
	keys, e := r.trusted()
	if e != nil || keys[r.SigningKeyID] == nil {
		return retrieval.ErrTrust
	}
	ids := map[string]bool{}
	for _, p := range r.PACS {
		if !retrieval.Opaque(p.ID) || ids[p.ID] || !retrieval.Identifier(p.PatientIssuer) || !retrieval.Identifier(p.AccessionIssuer) {
			return retrieval.ErrPolicy
		}
		ids[p.ID] = true
		if validateEndpointURL("dicomwebUrl", p.DICOMwebURL, "https", "") != nil {
			return retrieval.ErrPolicy
		}
		u, _ := url.Parse(p.DICOMwebURL)
		if u.RawPath != "" || u.Path == "/" || filepath.Clean(u.Path) != u.Path {
			return retrieval.ErrPolicy
		}
		if p.Adapter != "dicomweb-qido-wado-v1" || p.MaxInstanceBytes < 1 || p.MaxInstanceBytes > maxDICOMRequestBytes || p.MaxStudyBytes < p.MaxInstanceBytes || p.MaxStudyBytes > 64*maxDICOMRequestBytes || p.MaxInstances < 1 || p.MaxInstances > 1000000 || p.RequestTimeoutSeconds < 1 || p.RequestTimeoutSeconds > 300 {
			return retrieval.ErrPolicy
		}
	}
	ids = map[string]bool{}
	for _, p := range r.Policies {
		if !retrieval.Opaque(p.ID) || ids[p.ID] || len(p.SourceAddresses) < 1 || len(p.SourceAddresses) > 64 || !retrieval.Identifier(p.SendingApplication) || !retrieval.Identifier(p.SendingFacility) {
			return retrieval.ErrPolicy
		}
		ids[p.ID] = true
		if _, ok := r.pacs(p.PACSID); !ok {
			return retrieval.ErrPolicy
		}
		if !retrieval.AccessionSource(p.AccessionSource) || len(p.OrderControls) < 1 {
			return retrieval.ErrPolicy
		}
		for _, c := range p.OrderControls {
			if c != "NW" && c != "XO" && c != "CA" && c != "DC" {
				return retrieval.ErrPolicy
			}
		}
		for _, s := range p.SourceAddresses {
			a, e := netip.ParseAddr(s)
			if e != nil || a.IsUnspecified() || a.IsMulticast() {
				return retrieval.ErrPolicy
			}
		}
	}
	return nil
}
func retrievalEnabledLocal(cfg *config) bool { return cfg.Retrieval != nil && cfg.Retrieval.Enabled }
func authorizeRetrieval(cfg *config, envelope string) (retrieval.Permit, retrievalPACS, error) {
	if !retrievalEnabledLocal(cfg) || validateRetrievalConfig(cfg) != nil {
		return retrieval.Permit{}, retrievalPACS{}, retrieval.ErrPolicy
	}
	keys, e := cfg.Retrieval.trusted()
	if e != nil {
		return retrieval.Permit{}, retrievalPACS{}, e
	}
	p, _, e := retrieval.Verify(envelope, keys)
	if e != nil {
		return p, retrievalPACS{}, e
	}
	pacs, ok := cfg.Retrieval.pacs(p.PACSID)
	policy, approved := cfg.Retrieval.policy(p.SourcePolicyID)
	if !ok || !approved || policy.PACSID != p.PACSID || p.CompanyID != cfg.Retrieval.CompanyID || p.ConnectorID != cfg.RelayID || p.Patient.Issuer != pacs.PatientIssuer || p.Examination.Issuer != pacs.AccessionIssuer || p.Examination.AccessionSource != policy.AccessionSource || (p.Patient.IssuerSource == "local" && !policy.AllowMissingPatientIssuer) {
		return p, pacs, retrieval.ErrPolicy
	}
	return p, pacs, nil
}
func retrievalCapability(cfg *config) any {
	keys, _ := cfg.Retrieval.trusted()
	ids := make([]string, 0, len(keys))
	for k := range keys {
		ids = append(ids, k)
	}
	slices.Sort(ids)
	pacs := make([]string, 0, len(cfg.Retrieval.PACS))
	for _, p := range cfg.Retrieval.PACS {
		pacs = append(pacs, p.ID)
	}
	slices.Sort(pacs)
	return map[string]any{"version": 2, "readinessRule": "PACS_OR_ORDER", "trustedKeyIds": ids, "pacsIds": pacs}
}

const permitKeyFilename = "permit-signing-key.json"

type permitKeyFile struct {
	Seed      string `json:"seed"`
	PublicKey string `json:"publicKey"`
	KeyID     string `json:"keyId"`
}

// Explicit offline provisioning only. O_EXCL prevents rotation or lost-key recovery
// from silently replacing an existing signing authority.
func generatePermitKey(configPath string) error {
	d, e := openSafeDirectory(filepath.Dir(configPath))
	if e != nil {
		return errors.New("permit_key_directory_unavailable")
	}
	defer d.Close()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return errors.New("permit_key_generation_failed")
	}
	defer zeroBytes(key)
	data, e := json.Marshal(permitKeyFile{base64.RawURLEncoding.EncodeToString(key.Seed()), base64.RawURLEncoding.EncodeToString(pub), permitKeyID(pub)})
	if e != nil {
		return e
	}
	defer zeroBytes(data)
	f, e := createRelativeFile(d.file, permitKeyFilename, 0600, filepath.Join(filepath.Dir(configPath), permitKeyFilename))
	if e != nil {
		return errors.New("permit_key_creation_refused")
	}
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return errors.New("permit_key_write_failed")
	}
	return nil
}
func readPermitSigningKey(cfg *config) (ed25519.PrivateKey, error) {
	directory, e := openSafeDirectory(filepath.Dir(cfg.configPath))
	if e != nil {
		return nil, errors.New("permit_key_unavailable")
	}
	defer directory.Close()
	f, e := directory.open(permitKeyFilename)
	if e != nil {
		return nil, errors.New("permit_key_unavailable")
	}
	defer f.Close()
	if runtime.GOOS != "windows" {
		info, e := f.Stat()
		dir, dirErr := directory.file.Stat()
		if e != nil || dirErr != nil || info.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700 {
			return nil, errors.New("permit_key_permissions_invalid")
		}
	}
	data, e := readBoundedFile(f, 4096)
	if e != nil {
		return nil, errors.New("permit_key_unavailable")
	}
	defer zeroBytes(data)
	var record permitKeyFile
	if retrieval.StrictJSON(data, &record) != nil {
		return nil, retrieval.ErrTrust
	}
	seed, e := retrieval.DecodeBase64(record.Seed, 32)
	if e != nil {
		return nil, retrieval.ErrTrust
	}
	defer zeroBytes(seed)
	key := ed25519.NewKeyFromSeed(seed)
	pub := key.Public().(ed25519.PublicKey)
	keys, e := cfg.Retrieval.trusted()
	if e != nil || record.KeyID != cfg.Retrieval.SigningKeyID || permitKeyID(pub) != record.KeyID || record.PublicKey != base64.RawURLEncoding.EncodeToString(pub) || !bytes.Equal(keys[record.KeyID], pub) {
		zeroBytes(key)
		return nil, retrieval.ErrTrust
	}
	return key, nil
}
