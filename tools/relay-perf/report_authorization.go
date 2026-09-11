package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"net"
	"strings"

	"github.com/telrad-au/relay/internal/retrieval"
)

// Performance fixtures use an ephemeral signing authority in the private worker
// configuration. Private local state receives the same ephemeral test key.
// This isolates report transport measurements from order-ingestion measurements.
func syntheticReportKey(cfg workerConfig) (ed25519.PrivateKey, string) {
	seed, err := base64.RawURLEncoding.DecodeString(cfg.ReportSigningSeed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, ""
	}
	key := ed25519.NewKeyFromSeed(seed)
	return key, "ed25519-" + hash(key.Public().(ed25519.PublicKey))
}
func syntheticReportKeyRecord(cfg workerConfig) any {
	key, kid := syntheticReportKey(cfg)
	if key == nil {
		return nil
	}
	return map[string]string{"seed": cfg.ReportSigningSeed, "publicKey": base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), "keyId": kid}
}
func syntheticReportPermit(cfg workerConfig) string {
	key, kid := syntheticReportKey(cfg)
	if key == nil {
		return ""
	} // In-memory server unit fixtures need no Relay identity.
	host := "ris"
	if cfg.RIS != "" {
		host, _, _ = net.SplitHostPort(cfg.RIS)
	}
	p := retrieval.ReportPermit{Version: 1, Purpose: "report-delivery", ProcessingID: "P", ConnectorID: "perf-relay", SourcePolicyID: "hl7-listener", Examination: retrieval.Examination{Accession: "RELAY-REPORT", Issuer: "perf-relay", AccessionSource: "OBR-18"}, Procedure: retrieval.Procedure{Sequence: 1, SourceSetID: "1"}, HL7SHA256: strings.Repeat("0", 64), IssuedAt: "2000-01-01T00:00:00.000Z", ReportHost: host, ReportPort: 2576}
	permit, _ := retrieval.SignReport(p, kid, key)
	return permit
}
