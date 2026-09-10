package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

func referralURL(cfg *config, path string) string {
	// Config validation already binds all transport endpoints to this origin.
	return strings.TrimSuffix(cfg.ControlURL, "/control") + path
}
func referralSegments(message []byte) ([][]string, error) {
	if _, e := hl7ControlID(message); e != nil {
		return nil, retrieval.ErrPolicy
	}
	var result [][]string
	for remaining := message; len(remaining) > 0; {
		segment, rest := nextHL7Segment(remaining)
		remaining = rest
		if len(segment) == 0 {
			continue
		}
		if len(segment) < 4 || segment[3] != '|' {
			return nil, retrieval.ErrPolicy
		}
		result = append(result, strings.Split(string(segment), "|"))
	}
	if len(result) == 0 || result[0][0] != "MSH" || field(result[0], 1) != "^~\\&" {
		return nil, retrieval.ErrPolicy
	}
	return result, nil
}
func field(s []string, i int) string {
	if i >= len(s) {
		return ""
	}
	return s[i]
}

type orderSourcePolicy struct {
	referralPolicy
	AccessionIssuer string
}

func referralScopes(cfg *config, peer net.Addr, message []byte) ([]retrieval.Permit, bool, error) {
	policies := make([]orderSourcePolicy, 0, len(cfg.Retrieval.Policies))
	for _, policy := range cfg.Retrieval.Policies {
		pacs, _ := cfg.Retrieval.pacs(policy.PACSID)
		policies = append(policies, orderSourcePolicy{policy, pacs.AccessionIssuer})
	}
	return orderScopes(cfg.Retrieval.CompanyID, cfg.Retrieval.ConnectorID, policies, peer, message)
}

// Share source-approved order parsing across the two separately signed purposes.
func orderScopes(companyID, connectorID string, policies []orderSourcePolicy, peer net.Addr, message []byte) ([]retrieval.Permit, bool, error) {
	segments, e := referralSegments(message)
	if e != nil {
		return nil, false, e
	}
	msh := segments[0]
	if field(msh, 8) != "ORM^O01" && field(msh, 8) != "ORM^O01^ORM_O01" {
		return nil, false, nil
	}
	if field(msh, 11) != "2.3.1" && field(msh, 11) != "2.5" {
		return nil, true, retrieval.ErrPolicy
	}
	// Processing ID must be explicit; TEST referrals can go to the isolated
	// cloud lane, but TEST control sessions never start a PACS request.
	if field(msh, 10) != "P" && field(msh, 10) != "T" {
		return nil, true, retrieval.ErrPolicy
	}
	host, _, e := net.SplitHostPort(peer.String())
	if e != nil {
		return nil, true, retrieval.ErrPolicy
	}
	addr, e := netip.ParseAddr(host)
	if e != nil {
		return nil, true, retrieval.ErrPolicy
	}
	var policy orderSourcePolicy
	matches := 0
	for _, p := range policies {
		approved := false
		for _, s := range p.SourceAddresses {
			a, _ := netip.ParseAddr(s)
			if a.Unmap() == addr.Unmap() {
				approved = true
			}
		}
		if approved && field(msh, 2) == p.SendingApplication && field(msh, 3) == p.SendingFacility {
			policy = p
			matches++
		}
	}
	if matches != 1 {
		return nil, true, retrieval.ErrPolicy
	}
	return parsedOrderScopes(companyID, connectorID, policy, message, segments)
}

// Decode clinical scope independently of retrieval's optional source approval.
func parsedOrderScopes(companyID, connectorID string, policy orderSourcePolicy, message []byte, segments [][]string) ([]retrieval.Permit, bool, error) {
	msh := segments[0]
	if field(msh, 8) != "ORM^O01" && field(msh, 8) != "ORM^O01^ORM_O01" {
		return nil, false, nil
	}
	if (field(msh, 11) != "2.3.1" && field(msh, 11) != "2.5") || (field(msh, 10) != "P" && field(msh, 10) != "T") {
		return nil, true, retrieval.ErrPolicy
	}
	var pid []string
	var obrs [][]string
	var orcs [][]string
	var orc []string
	controls := map[string]bool{}
	for i, s := range segments {
		switch s[0] {
		case "MSH":
			if i != 0 {
				return nil, true, retrieval.ErrPolicy
			}
		case "PID":
			if pid != nil {
				return nil, true, retrieval.ErrPolicy
			}
			pid = s
		case "ORC":
			orc = s
			control := field(s, 1)
			approved := false
			for _, c := range policy.OrderControls {
				if control == c {
					approved = true
				}
			}
			if !approved {
				return nil, true, retrieval.ErrPolicy
			}
			controls[control] = true
		case "OBR":
			if orc == nil {
				return nil, true, retrieval.ErrPolicy
			}
			obrs = append(obrs, s)
			orcs = append(orcs, orc)
		}
	}
	// The cloud applies whole-order cancellation. Mixed control messages cannot
	// accidentally cancel some procedures and authorize others.
	if len(controls) != 1 {
		return nil, true, retrieval.ErrPolicy
	}
	if controls["CA"] || controls["DC"] {
		return []retrieval.Permit{}, true, nil
	}
	if len(obrs) == 0 || len(obrs) > 64 {
		return nil, true, retrieval.ErrPolicy
	}
	sum := sha256.Sum256(message)
	permits := make([]retrieval.Permit, 0, len(obrs))
	mapping := strings.Split(policy.AccessionSource, "-")
	index, _ := strconv.Atoi(mapping[1])
	for i, obr := range obrs {
		segment := obr
		if mapping[0] == "ORC" {
			segment = orcs[i]
		}
		accession := field(segment, index)
		if index != 18 {
			parts := strings.Split(accession, "^")
			if len(parts) > 2 || (field(parts, 1) != "" && field(parts, 1) != policy.AccessionIssuer) {
				return nil, true, retrieval.ErrPolicy
			}
			accession = field(parts, 0)
		}
		setID := field(obr, 1)
		if setID == "" {
			setID = strconv.Itoa(i + 1)
		}
		p := retrieval.Permit{Version: 2, Purpose: "pacs-retrieval", Kind: "accession", CompanyID: companyID, ConnectorID: connectorID, PACSID: policy.PACSID, SourcePolicyID: policy.ID, Examination: retrieval.Examination{Accession: accession, Issuer: policy.AccessionIssuer, AccessionSource: policy.AccessionSource}, Procedure: retrieval.Procedure{Sequence: i + 1, SourceSetID: setID}, HL7SHA256: hex.EncodeToString(sum[:]), IssuedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
		permits = append(permits, p)
	}
	return permits, true, nil
}
func signReferrals(cfg *config, peer net.Addr, message []byte) ([]string, bool, error) {
	scopes, qualifies, err := referralScopes(cfg, peer, message)
	if err != nil || !qualifies {
		return nil, qualifies, err
	}
	permits := []string{}
	if len(scopes) == 0 {
		return permits, true, nil
	}
	key, err := readPermitSigningKey(cfg)
	if err != nil {
		return nil, true, err
	}
	defer zeroBytes(key)
	for _, p := range scopes {
		value, err := retrieval.Sign(p, cfg.Retrieval.SigningKeyID, key)
		if err != nil {
			return nil, true, err
		}
		permits = append(permits, value)
	}
	return permits, true, nil
}

func ingestClinicHL7(ctx context.Context, cfg *config, peer net.Addr, client *http.Client, provider *credentialProvider, status *runtimeStatusManager, message []byte, controlID string) ([]byte, error) {
	segments, err := referralSegments(message)
	if err != nil {
		return nil, err
	}
	kind := field(segments[0], 8)
	if kind != "ORM^O01" && kind != "ORM^O01^ORM_O01" {
		return ingestHL7(ctx, cfg.HL7URL, client, provider, status, message, controlID)
	}
	reportCfg, err := currentReportConfig(cfg)
	if err != nil {
		return nil, err
	}
	reports, err := signReportAuthorizations(reportCfg, message)
	if err != nil {
		return nil, err
	}
	permits := []string{}
	key, err := readReportSigningKey(reportCfg)
	if err != nil {
		return nil, err
	}
	pub := key.Public().(ed25519.PublicKey)
	zeroBytes(key)
	registrationKeys := map[string]ed25519.PublicKey{permitKeyID(pub): pub}
	if retrievalEnabledLocal(cfg) {
		current, err := currentRetrievalConfig(cfg)
		if err != nil {
			return nil, err
		}
		routing, err := discoverRetrievalSettings(ctx, current, client, provider, status)
		if err != nil {
			return nil, err
		}
		if field(segments[0], 10) == "T" && routing.IngestMode != "TEST" {
			return nil, retrieval.ErrPolicy
		}
		if routing.Mode == "RETRIEVE" {
			if !routing.Available && routing.IngestMode != "TEST" {
				return nil, errors.New("retrieval_not_enabled")
			}
			permits, _, err = signReferrals(current, peer, message)
			if err != nil {
				return nil, err
			}
			keys, _ := current.Retrieval.trusted()
			registrationKeys[current.Retrieval.SigningKeyID] = keys[current.Retrieval.SigningKeyID]
		}
	}
	for kid, public := range registrationKeys {
		if len(reports)+len(permits) == 0 {
			continue
		}
		code, _, err := controlRequest(ctx, client, provider, http.MethodPost, referralURL(cfg, "/signing-keys"), map[string]string{"keyId": kid, "publicKey": base64.RawURLEncoding.EncodeToString(public)}, nil)
		if err != nil || (code != 200 && code != 201) {
			controlFailure(status, code)
			return nil, errors.New("permit_key_registration_failed")
		}
	}
	body, err := json.Marshal(struct {
		Version              int      `json:"version"`
		HL7                  string   `json:"hl7"`
		Permits              []string `json:"permits"`
		ReportAuthorizations []string `json:"reportAuthorizations"`
	}{2, base64.StdEncoding.EncodeToString(message), permits, reports})
	if err != nil {
		return nil, err
	}
	// Commit the order and both kinds of authorization under one immutable ACK.
	return ingestHL7Body(ctx, referralURL(cfg, "/ingest/referrals"), client, provider, status, body, controlID, "application/json")
}

// Policy edits take effect before signing, every PACS operation, and lease
// renewal. A changed connector/origin fences active work until service restart.
func currentRetrievalConfig(cfg *config) (*config, error) {
	if cfg.configPath == "" {
		if validateRetrievalConfig(cfg) != nil {
			return nil, retrieval.ErrPolicy
		}
		return cfg, nil
	}
	fresh, e := loadConfigMode(cfg.configPath, false)
	if e != nil || validateConfig(fresh, "run") != nil || !retrievalEnabledLocal(fresh) || fresh.RelayID != cfg.RelayID || fresh.ControlURL != cfg.ControlURL {
		return nil, retrieval.ErrPolicy
	}
	return fresh, nil
}

// The authenticated cloud selects routing only. Scope, identity, source policy,
// endpoints and trusted keys still come exclusively from clinic configuration.
type retrievalSettings struct {
	Version     int    `json:"version"`
	CompanyID   string `json:"companyId"`
	ConnectorID string `json:"connectorId"`
	IngestMode  string `json:"ingestMode"`
	Mode        string `json:"mode"`
	Available   bool   `json:"available"`
}

func discoverRetrievalSettings(ctx context.Context, cfg *config, client *http.Client, provider *credentialProvider, status *runtimeStatusManager) (retrievalSettings, error) {
	var result retrievalSettings
	var raw json.RawMessage
	code, _, err := controlRequest(ctx, client, provider, http.MethodGet, cfg.ControlURL+"/retrieval-settings", nil, &raw)
	if err == nil {
		var fields map[string]json.RawMessage
		if retrieval.StrictJSON(raw, &fields) != nil || len(fields) != 6 || (string(fields["available"]) != "true" && string(fields["available"]) != "false") || retrieval.StrictJSON(raw, &result) != nil {
			err = errors.New("invalid_control_response")
		}
	}
	if err != nil || code != 200 {
		controlFailure(status, code)
		return result, errors.New("retrieval_discovery_failed")
	}
	if result.Version != 2 || result.CompanyID != cfg.Retrieval.CompanyID || result.ConnectorID != cfg.Retrieval.ConnectorID || (result.Mode != "PUSH" && result.Mode != "RETRIEVE") || (result.IngestMode != "TEST" && result.IngestMode != "PRODUCTION") {
		return result, errors.New("retrieval_discovery_failed")
	}
	return result, nil
}
