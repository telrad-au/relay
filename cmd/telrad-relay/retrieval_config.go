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
	"path/filepath"
	"runtime"
	"slices"
	"strings"

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
	ID                    string   `json:"id"`
	Host                  string   `json:"host,omitempty"`
	Port                  int      `json:"port,omitempty"`
	CalledAETitle         string   `json:"calledAETitle,omitempty"`
	CallingAETitle        string   `json:"callingAETitle,omitempty"`
	StorageSOPClasses     []string `json:"storageSopClasses,omitempty"`
	AccessionIssuer       string   `json:"accessionIssuer"`
	MaxStudyBytes         int64    `json:"maxStudyBytes"`
	MaxInstanceBytes      int64    `json:"maxInstanceBytes"`
	MaxInstances          int      `json:"maxInstances"`
	RequestTimeoutSeconds int      `json:"requestTimeoutSeconds"`
	// This DIMSE profile has no completion API. Other PACS profiles need a
	// qualified adapter; this is not an operator-selectable completion policy.
	Adapter string `json:"adapter"`
}
type referralPolicy struct {
	ID                 string   `json:"id"`
	PACSID             string   `json:"pacsId"`
	SourceAddresses    []string `json:"sourceAddresses"`
	SendingApplication string   `json:"sendingApplication"`
	SendingFacility    string   `json:"sendingFacility"`
	OrderControls      []string `json:"orderControls"`
	AccessionSource    string   `json:"accessionSource"`
}

func permitKeyID(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return "ed25519-" + hex.EncodeToString(sum[:])
}
func (r *retrievalConfig) trusted() (map[string]ed25519.PublicKey, error) {
	return trustedOrderKeys(r.TrustedPublicKeys)
}

func trustedOrderKeys(publicKeys []string) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	for _, s := range publicKeys {
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
		if !retrieval.Opaque(p.ID) || ids[p.ID] || !retrieval.Identifier(p.AccessionIssuer) {
			return retrieval.ErrPolicy
		}
		ids[p.ID] = true
		if p.Adapter != "dimse-find-get-v1" {
			return retrieval.ErrPolicy
		}
		if !validRetrievalHost(p.Host) || p.Port < 1 || p.Port > 65535 || !validRetrievalAE(p.CalledAETitle) || !validRetrievalAE(p.CallingAETitle) || len(p.StorageSOPClasses) < 1 || len(p.StorageSOPClasses) > 63 {
			return retrieval.ErrPolicy
		}
		classes := map[string]bool{}
		for _, class := range p.StorageSOPClasses {
			if !isStorageSOPClass(class) || classes[class] {
				return retrieval.ErrPolicy
			}
			classes[class] = true
		}
		if p.MaxInstanceBytes < 1 || p.MaxInstanceBytes > maxDICOMRequestBytes || p.MaxStudyBytes < p.MaxInstanceBytes || p.MaxStudyBytes > 64*maxDICOMRequestBytes || p.MaxInstances < 1 || p.MaxInstances > 1000000 || p.RequestTimeoutSeconds < 1 || p.RequestTimeoutSeconds > 300 {
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
	if !ok || !approved || policy.PACSID != p.PACSID || p.CompanyID != cfg.Retrieval.CompanyID || p.ConnectorID != cfg.RelayID || p.Examination.Issuer != pacs.AccessionIssuer || p.Examination.AccessionSource != policy.AccessionSource {
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
	return generateOrderKey(filepath.Dir(configPath), permitKeyFilename)
}
func generateOrderKey(directory, filename string) error {
	d, e := openSafeDirectory(directory)
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
	f, e := createRelativeFile(d.file, filename, 0600, filepath.Join(directory, filename))
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
	return syncDirectoryHandle(d.file)
}
func readPermitSigningKey(cfg *config) (ed25519.PrivateKey, error) {
	return readOrderSigningKey(cfg, cfg.Retrieval.SigningKeyID, cfg.Retrieval.TrustedPublicKeys)
}
func readOrderSigningKey(cfg *config, keyID string, publicKeys []string) (ed25519.PrivateKey, error) {
	key, e := readLocalOrderKey(filepath.Dir(cfg.configPath), permitKeyFilename)
	if e != nil {
		return nil, e
	}
	pub := key.Public().(ed25519.PublicKey)
	keys, e := trustedOrderKeys(publicKeys)
	if e != nil || permitKeyID(pub) != keyID || !bytes.Equal(keys[keyID], pub) {
		zeroBytes(key)
		return nil, retrieval.ErrTrust
	}
	return key, nil
}
func readLocalOrderKey(path, filename string) (ed25519.PrivateKey, error) {
	directory, e := openSafeDirectory(path)
	if e != nil {
		return nil, errors.New("permit_key_unavailable")
	}
	defer directory.Close()
	f, e := directory.open(filename)
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
	if permitKeyID(pub) != record.KeyID || record.PublicKey != base64.RawURLEncoding.EncodeToString(pub) {
		zeroBytes(key)
		return nil, retrieval.ErrTrust
	}
	return key, nil
}

func validRetrievalAE(s string) bool {
	return len(s) <= 16 && validAETitle([]byte(s)) && strings.TrimSpace(s) == s
}

func validRetrievalHost(s string) bool {
	if s == "" || strings.TrimSpace(s) != s {
		return false
	}
	if a, e := netip.ParseAddr(s); e == nil {
		return !a.IsUnspecified() && !a.IsMulticast() && a.Zone() == ""
	}
	if len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
