package main

import (
	"errors"
	"net"
	"net/netip"
	"strings"

	"github.com/telrad-au/relay/internal/retrieval"
)

type reportAuthorizationConfig struct {
	CompanyID         string               `json:"companyId"`
	ConnectorID       string               `json:"connectorId"`
	SigningKeyID      string               `json:"signingKeyId"`
	TrustedPublicKeys []string             `json:"trustedPublicKeys"`
	Policies          []reportSourcePolicy `json:"policies"`
}
type reportSourcePolicy struct {
	ID                 string   `json:"id"`
	SourceAddresses    []string `json:"sourceAddresses"`
	SendingApplication string   `json:"sendingApplication"`
	SendingFacility    string   `json:"sendingFacility"`
	AccessionSource    string   `json:"accessionSource"`
	AccessionIssuer    string   `json:"accessionIssuer"`
	OrderControls      []string `json:"orderControls,omitempty"`
}

// Share the clinic's existing source/key approval when retrieval is configured.
// Push-only clinics can approve report return without configuring a PACS.
func reportSigningConfig(cfg *config) (*reportAuthorizationConfig, error) {
	r := cfg.ReportAuthorization
	if r == nil {
		if !retrievalEnabledLocal(cfg) || validateRetrievalConfig(cfg) != nil {
			return nil, errors.New("report_authorization_not_configured")
		}
		source := cfg.Retrieval
		r = &reportAuthorizationConfig{
			CompanyID: source.CompanyID, ConnectorID: source.ConnectorID,
			SigningKeyID: source.SigningKeyID, TrustedPublicKeys: source.TrustedPublicKeys,
		}
		for _, p := range source.Policies {
			pacs, _ := source.pacs(p.PACSID)
			r.Policies = append(r.Policies, reportSourcePolicy{
				ID: p.ID, SourceAddresses: p.SourceAddresses,
				SendingApplication: p.SendingApplication, SendingFacility: p.SendingFacility,
				AccessionSource: p.AccessionSource, AccessionIssuer: pacs.AccessionIssuer,
				OrderControls: p.OrderControls,
			})
		}
	}
	if !retrieval.Opaque(r.CompanyID) || r.ConnectorID != cfg.RelayID || !retrieval.Opaque(r.ConnectorID) || len(r.TrustedPublicKeys) < 1 || len(r.TrustedPublicKeys) > 64 || len(r.Policies) < 1 || len(r.Policies) > 64 {
		return nil, retrieval.ErrPolicy
	}
	keys, err := trustedOrderKeys(r.TrustedPublicKeys)
	if err != nil || keys[r.SigningKeyID] == nil {
		return nil, retrieval.ErrTrust
	}
	// Both purposes use the single protected clinic signing key file.
	if retrievalEnabledLocal(cfg) && (r.CompanyID != cfg.Retrieval.CompanyID || r.SigningKeyID != cfg.Retrieval.SigningKeyID) {
		return nil, retrieval.ErrPolicy
	}
	ids := map[string]bool{}
	for _, p := range r.Policies {
		if !retrieval.Opaque(p.ID) || ids[p.ID] || !retrieval.Identifier(p.AccessionIssuer) || !retrieval.Identifier(p.SendingApplication) || !retrieval.Identifier(p.SendingFacility) || !retrieval.AccessionSource(p.AccessionSource) || len(p.SourceAddresses) < 1 || len(p.SourceAddresses) > 64 {
			return nil, retrieval.ErrPolicy
		}
		for _, control := range p.OrderControls {
			if control != "NW" && control != "XO" && control != "CA" && control != "DC" {
				return nil, retrieval.ErrPolicy
			}
		}
		ids[p.ID] = true
		for _, host := range p.SourceAddresses {
			addr, err := netip.ParseAddr(host)
			if err != nil || addr.IsUnspecified() || addr.IsMulticast() {
				return nil, retrieval.ErrPolicy
			}
		}
	}
	return r, nil
}
func currentReportConfig(cfg *config) (*config, error) {
	if cfg.configPath == "" {
		return cfg, nil
	}
	fresh, e := loadConfigMode(cfg.configPath, false)
	if e != nil || validateConfig(fresh, "run") != nil || fresh.RelayID != cfg.RelayID || fresh.ControlURL != cfg.ControlURL {
		return nil, retrieval.ErrPolicy
	}
	return fresh, nil
}
func signReportAuthorizations(cfg *config, peer net.Addr, message []byte) ([]string, error) {
	auth, e := reportSigningConfig(cfg)
	if e != nil {
		return nil, e
	}
	policies := make([]orderSourcePolicy, 0, len(auth.Policies))
	for _, p := range auth.Policies {
		controls := p.OrderControls
		if len(controls) == 0 {
			controls = []string{"NW", "XO", "CA", "DC"}
		}
		policies = append(policies, orderSourcePolicy{
			referralPolicy: referralPolicy{
				ID: p.ID, SourceAddresses: p.SourceAddresses,
				SendingApplication: p.SendingApplication, SendingFacility: p.SendingFacility,
				AccessionSource: p.AccessionSource, OrderControls: controls,
			},
			AccessionIssuer: p.AccessionIssuer,
		})
	}
	scopes, qualifies, e := orderScopes(auth.CompanyID, auth.ConnectorID, policies, peer, message)
	if e != nil {
		return nil, e
	}
	if !qualifies {
		return nil, retrieval.ErrPolicy
	}
	values := []string{}
	if len(scopes) == 0 {
		return values, nil
	}
	key, e := readOrderSigningKey(cfg, auth.SigningKeyID, auth.TrustedPublicKeys)
	if e != nil {
		return nil, e
	}
	defer zeroBytes(key)
	segments, _ := referralSegments(message)
	for _, p := range scopes {
		grant := retrieval.ReportPermit{
			Version: 1, Purpose: "report-delivery", ProcessingID: field(segments[0], 10),
			CompanyID: p.CompanyID, ConnectorID: p.ConnectorID, SourcePolicyID: p.SourcePolicyID,
			Examination: p.Examination, Procedure: p.Procedure,
			HL7SHA256: p.HL7SHA256, IssuedAt: p.IssuedAt,
			ReportHost: cfg.ReportHost, ReportPort: cfg.ReportPort,
		}
		value, e := retrieval.SignReport(grant, auth.SigningKeyID, key)
		if e != nil {
			return nil, e
		}
		values = append(values, value)
	}
	return values, nil
}
func authorizeReport(cfg *config, report reportMessage) error {
	current, e := currentReportConfig(cfg)
	if e != nil {
		return e
	}
	auth, e := reportSigningConfig(current)
	if e != nil {
		return e
	}
	keys, e := trustedOrderKeys(auth.TrustedPublicKeys)
	if e != nil {
		return e
	}
	p, e := retrieval.VerifyReport(report.Authorization, keys)
	if e != nil {
		return e
	}
	if p.ProcessingID != "P" || p.ReportHost != cfg.ReportHost || p.ReportPort != cfg.ReportPort || p.CompanyID != auth.CompanyID || p.ConnectorID != current.RelayID || p.ReportHost != current.ReportHost || p.ReportPort != current.ReportPort {
		return retrieval.ErrPolicy
	}
	approved := false
	for _, policy := range auth.Policies {
		if policy.ID == p.SourcePolicyID && policy.AccessionIssuer == p.Examination.Issuer && policy.AccessionSource == p.Examination.AccessionSource {
			approved = true
		}
	}
	if !approved {
		return retrieval.ErrPolicy
	}
	if !safeRetrievalReport(report.Payload) {
		return retrieval.ErrPolicy
	}
	segments, e := referralSegments([]byte(report.Payload))
	if e != nil {
		return e
	}
	if field(segments[0], 10) != "P" {
		return retrieval.ErrPolicy
	}
	count := 0
	for _, s := range segments[1:] {
		if s[0] == "OBR" {
			count++
			if field(s, 18) != p.Examination.Accession || strings.ContainsAny(field(s, 18), "^~\\&") {
				return retrieval.ErrIdentity
			}
			// The cloud report contract uses OBR-18 exclusively. Ambiguous alternate
			// identifiers must not let a downstream RIS select an unrelated accession.
			if field(s, 2) != "" || field(s, 3) != "" {
				return retrieval.ErrIdentity
			}
		}
		if s[0] == "ORC" && (field(s, 2) != "" || field(s, 3) != "") {
			return retrieval.ErrIdentity
		}
	}
	if count != 1 {
		return retrieval.ErrIdentity
	}
	return nil
}
