# Relay testing

The default suite uses local loopback TLS/HTTP, WSS, MLLP, and byte-level DICOM
fixtures. Tests contain synthetic identifiers only.

```bash
go test -race ./...
go vet ./...
govulncheck ./...
scripts/check-licenses.sh
scripts/check-publication.sh
```

Some managed development environments require permission for loopback sockets;
that is an environment restriction, not a product failure.

## Contract coverage

Pairing and credentials cover HTTP `201`, locally derived endpoint paths,
legacy endpoint equality, content types, redirect rejection, authorization
cardinality, response body bounds, secret-safe errors, file modes, transaction
recovery, overlap expiry, atomic rotation, and live provider adoption.

HL7 coverage keeps one clinic MLLP socket across sequential exchanges and
asserts one HTTPS request per message. It checks exact framing, UTF-8,
`MSH-10`/`MSA-2` correlation, `AA`/`AE`/`AR`, byte-for-byte ACK return, limits,
concurrency, cancellation, retryable statuses, both `Retry-After` forms, and
reuse of the exact request body and idempotency key.

Report-return coverage checks durable pending and terminal transitions,
accepted-delivery deduplication without resend, ambiguous restart behavior,
metadata-reuse rejection, storage-failure rollback, retention and capacity,
fail-closed corruption handling, and one-time migration from the legacy JSON
ledger without retaining plaintext tokens or control IDs. Ledger transition
benchmarks exercise 100, 1,000, and 10,000 retained records and report allocated
database-page bytes and write calls per transition.

```bash
go test ./cmd/telrad-relay -run '^$' \
  -bench '^BenchmarkReportLedgerTransitions$' -benchmem
```

DICOM fixtures construct UL PDUs and DIMSE command sets directly. Tests cover
association acceptance/rejection, presentation-context choice, C-ECHO,
C-STORE, release/abort, multiple sequential stores, command and PDU bounds,
deterministic Part 10 file meta, every supported transfer syntax, unchanged
dataset bytes, repeated identical and byte-different arrivals sharing a SOP
Instance UID, the absence of DICOM idempotency headers or hidden retries, fresh
HTTP `201` receipts, DIMSE status mappings, backpressure, disconnect
cancellation, total size accounting, drain behavior, and the rule that success
cannot precede a valid cloud receipt.

The integration cloud uses TLS/WSS bearer authentication. No fixture contains a
private certificate authority, client identity, custom ALPN, or raw TCP ingest
proxy.

## Preview conformance

Preview conformance is opt-in and PHI-free:

```bash
TELRAD_RELAY_PREVIEW_TEST=1 \
TELRAD_RELAY_PREVIEW_CREDENTIAL='trr_v1_...' \
go test ./cmd/telrad-relay -run TestPreviewConformance -count=1
```

If the credential is absent or malformed, the test explicitly reports that it
was unavailable and skips. Never place the credential in source, shell history,
CI logs, or a checked-in environment file.

## Release matrix

Before a release, also verify CGO-disabled Linux amd64, Linux arm64, and Windows
amd64 builds; Docker build and Compose rendering; hosted installer tests;
signed and unsigned bundle rejection/acceptance; piped Windows installer
execution and current-session PATH activation; the Windows Authenticode,
timestamp, and signer-pin verifier; Ed25519 manifest verification; and
publication/licence audits. These
checks build artifacts only and do not authorize publishing, tagging, deploying,
or promoting an image.

CI exercises `scripts/build-signed-release.sh` with an ephemeral self-signed PFX
only to test the Authenticode, installer-pin, and bundle-finalization contracts.
The production workflow never uses that PFX path: Azure Artifact Signing signs
the Windows executable before `scripts/finalize-signed-release.sh` applies the
Relay update signatures.
