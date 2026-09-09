# Relay testing

The default suite uses local loopback TLS/HTTP, MLLP, and byte-level DICOM
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

MLLP framing and HL7 field-parser benchmarks cover the 1 KiB through 8 MiB
configuration range, plus representative ACK sizes up to the 64 KiB cloud
response limit:

```bash
go test ./cmd/telrad-relay -run '^$' \
  -bench '^(BenchmarkReadMLLPFrame|BenchmarkHL7ControlID|BenchmarkHL7Acknowledgement)$' \
  -benchmem
```

Report-return coverage checks authenticated HTTPS session creation, trusted
transport metadata, lost result responses, identical result replay without
another MLLP send, retransmission under a new cloud claim, correlated application
ACKs, shutdown draining, and absence of a local ledger. Schema upgrade tests
check credential preservation, idempotence and rejection of foreign endpoints.
The Telrad API conformance suite separately covers atomic claims, lease expiry,
late and superseded results, idempotent failure accounting, session replacement,
TEST mode, and manual retry routing.

DICOM fixtures construct UL PDUs and DIMSE command sets directly. Tests cover
acceptance of arbitrary valid called AE titles, existing whitespace-padding
tolerance, rejection of blank titles and invalid characters within titles,
exact AE title echo in association responses, presentation-context choice, C-ECHO,
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
VR Little Endian plus JPEG Lossless SV1 over a real TCP C-STORE association
addressed to `CLINIC_ARCHIVE` to exercise a called AE title other than `TELRAD`.
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
message while retaining the same idempotency key. CI also validates the shared
ORU^R01 and ACK fixtures against checked-in HL7 v2.5 conformance profiles with
HAPI HL7 2.6.0 in the test-only image
`maven:3.9.11-eclipse-temurin-21@sha256:6fdc855a6ed81d288ca7ca37ac6ff5e9308b612485c0801d70b25a858c83d237`.
The validator checks the report and application acknowledgements, and proves
that the previous missing and shifted OBR/OBX fields are rejected.

Run the independent fixture validation locally with:

```bash
scripts/check-hl7-fixtures.sh
```

The integration cloud uses HTTPS bearer authentication. No fixture contains a
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

## Native privilege boundaries

The suite covers malformed and injected recovery paths, link replacement races,
read-only command behavior, unauthorized/malformed local IPC, serialized credential
operations, unpaired management startup, and independent signature verification at
the update privilege boundary. Windows adds Known Folder environment isolation,
ancestor-junction rejection, private backup creation ACLs, replacement while the
original CLI image remains running, and identification-only named-pipe client tests.
Fresh Windows directory tests use a parent with inherited public write access and
verify that managed directories are protected at creation; existing unsafe
installation directories and junctions remain rejected without permission changes.
ACL repair tests cover real file and directory handles, reject hard-linked targets,
and verify that directory repair does not change existing child permissions.
A real PowerShell subprocess test verifies that nested installation errors remain
plain text so an outer PowerShell installer can capture the failure.

CI uses `scripts/check-native-installation.sh` on its disposable Linux runner and
`packaging/install-native.Tests.ps1` on its disposable Windows runner. These install
at the real managed paths and exercise service identities, unpaired startup,
configuration preservation, stopped-service repair, and malicious state links.
The installed lifecycle test pairs against a synthetic HTTPS server, rotates and
replaces the live identity, installs a signed candidate, and verifies rollback
and enrollment preservation when a signed candidate reports the wrong version.
The failing update must finish rollback before returning a nonzero result. Native
tests also exercise service-setting/link restoration and fresh Windows installation
rollback after firewall setup fails.
The Windows rollback check requires the firewall-specific error so an earlier
installation failure cannot pass it. The disposable Ubuntu CI runner restores
root ownership and mode `0755` on `/usr/local/bin`, which its image makes
world-writable for npm. The production installer still rejects writable paths.
The lifecycle test's temporary CA stays on the disposable host and is removed afterward.
They require `TELRAD_NATIVE_INSTALL_TEST=1`; do not run them on a clinic host.
Cross-compilation alone does not validate SCM, UAC, Windows ACLs, or systemd.

The Linux coverage gate combines race-enabled unit and installed-process coverage
using Go's `covdata merge -pcombine` command. Both collect binary coverage data;
the existing 69% total threshold still applies.

Container builds use `-tags relay_container`. CI checks the dependency exclusion,
non-root UID, read-only root filesystem, and absence of native-service assumptions.

## Retrieval v2

The default race suite exercises the real strict Go verifier with the shared
synthetic v2 vectors, independently signed malformed data, trust retirement,
source-IP restrictions, multiple OBRs, cancellation, exact MLLP ACKs, immutable
transport retries, per-attempt study confirmation, multipart/object validation,
separate duplicate uploads, failed receipts, expiry cancellation and restart
with later matching Study UIDs. Configuration migration keeps push defaults.
Fixtures contain synthetic public verification vectors only; private test keys
are generated at runtime. The vector seed is intentionally excluded.

Run actual-binary Orthanc interoperability on a disposable Linux Docker host:

```sh
TELRAD_RETRIEVAL_ORTHANC_TEST=1 go test -race ./cmd/telrad-relay \
  -run '^TestRetrievalOrthancBinaryRestart$' -count=1 -timeout=4m -v
```

This builds and starts the actual Relay process, runs real Orthanc QIDO/WADO
through a loopback TLS endpoint, sends an MLLP referral, receives the ACK, observes
study selection and uploads, stops Relay, adds another matching study, and repeats
using the original permit. Relay's inbound DICOM port is deliberately unavailable;
the listener is disabled. Only credentials, configuration, key and nonclinical
runtime status survive. The container uses the pinned Orthanc image documented
above with the DICOMweb plugin, full study metadata and no inbound DICOM service.
All fixture containers are removed. This test uses a synthetic cloud HTTP server.

**Activation gate:** passing this harness and the real cloud conformance suite
separately does not qualify their combined deployment. Before enabling retrieval,
run the actual Relay against the real cloud HTTPS ingestion/definitive receipt
pipeline and this PACS profile, including restart and late studies. Verify cloud
readiness, not merely an uploaded result. Record the exact source commit, binary
version/digest, PACS version/configuration, study/inventory outcomes and recovery
review. Qualify vendor completion evidence, in-progress/error states and expected
SOP mismatch on any adapter that provides that information; the initial generic
DICOMweb adapter supports only completion-unavailable/order fallback.

The native Linux/Windows install-and-rollback jobs and container build remain
required before release. Cross-builds and simulated installer tests do not prove
Windows SCM/ACL or installed systemd behavior on a production clinic host.
