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

CI also runs a blocking Orthanc-backed interoperability test in the required
`Test and build` job. It uses the test-only image
`jodogne/orthanc-plugins:1.12.11@sha256:e7bffe0351cd391eacab8e78098e236efe6cafed987830e9b462b2050a0eae4a`,
creates deterministic PHI-free Secondary Capture fixtures, and sends Explicit
VR Little Endian plus JPEG Lossless SV1 over a real TCP C-STORE association.
The harness records fragmented dataset PDVs before Relay, compares them
byte-for-byte with the dataset in Relay's HTTPS Part 10 body, and imports the
result into a second clean Orthanc instance to validate the SOP identifiers,
transfer syntax, and representative tags. A separate test-only dcm4che
5.33.1 image pinned at
`dcm4che/dcm4che-tools:5.33.1@sha256:c8fbede4a6cf6047370ad21ce12fcc6be7ab013ff4996f1d032eb55239f870ed`
validates the captured objects against the checked-in Secondary Capture IOD
profile and independently decodes both transfer syntaxes. The decoded pixels
must exactly match the deterministic 512x512 source image. Relay returns
C-STORE success only after the fake HTTPS cloud supplies a valid receipt.

The Docker-backed test is opt-in locally and has bounded startup, execution,
and cleanup timeouts:

```bash
TELRAD_ORTHANC_INTEROP_TEST=1 \
go test -race ./cmd/telrad-relay \
  -run '^TestOrthancDICOMPayloadIntegrity$' -count=1 -timeout=5m
```

HL7 listener coverage sends a non-trivial synthetic UTF-8 message through a
real TCP/MLLP connection and asserts that the HTTPS request body is exactly the
original message without its MLLP envelope. Retry coverage separately asserts
that the first HTTPS body and every retry remain byte-identical to that original
message while retaining the same idempotency key.

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
