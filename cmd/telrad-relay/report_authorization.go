package main

import (
	"crypto/ed25519"
	"strings"

	"github.com/telrad-au/relay/internal/retrieval"
)

// Every order received on the clinic HL7 listener is eligible. The listener is
// the trust boundary; no cloud response can create a locally signed order.
const reportSourceID = "hl7-listener"

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
func signReportAuthorizations(cfg *config, message []byte) ([]string, error) {
	segments, e := referralSegments(message)
	if e != nil {
		return nil, e
	}
	policy := orderSourcePolicy{referralPolicy: referralPolicy{
		ID: reportSourceID, AccessionSource: "OBR-18", OrderControls: []string{"NW", "XO", "CA", "DC"},
	}, AccessionIssuer: cfg.RelayID}
	scopes, qualifies, e := parsedOrderScopes("", cfg.RelayID, policy, message, segments)
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
	key, e := readReportSigningKey(cfg)
	if e != nil {
		return nil, e
	}
	defer zeroBytes(key)
	for _, p := range scopes {
		grant := retrieval.ReportPermit{
			Version: 1, Purpose: "report-delivery", ProcessingID: field(segments[0], 10),
			ConnectorID: p.ConnectorID, SourcePolicyID: p.SourcePolicyID,
			Examination: p.Examination, Procedure: p.Procedure,
			HL7SHA256: p.HL7SHA256, IssuedAt: p.IssuedAt,
			ReportHost: cfg.ReportHost, ReportPort: cfg.ReportPort,
		}
		value, e := retrieval.SignReport(grant, permitKeyID(key.Public().(ed25519.PublicKey)), key)
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
	key, e := readReportSigningKey(current)
	if e != nil {
		return e
	}
	defer zeroBytes(key)
	pub := key.Public().(ed25519.PublicKey)
	keys := map[string]ed25519.PublicKey{permitKeyID(pub): pub}
	p, e := retrieval.VerifyReport(report.Authorization, keys)
	if e != nil {
		return e
	}
	if p.ProcessingID != "P" || p.ReportHost != cfg.ReportHost || p.ReportPort != cfg.ReportPort || p.ConnectorID != current.RelayID || p.ReportHost != current.ReportHost || p.ReportPort != current.ReportPort {
		return retrieval.ErrPolicy
	}
	if p.SourcePolicyID != reportSourceID || p.Examination.Issuer != current.RelayID || p.Examination.AccessionSource != "OBR-18" {
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
