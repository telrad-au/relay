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

## Azure Artifact Signing integration

After configuring the `artifact-signing-test` GitHub Environment described in
`docs/releases.md`, explicitly dispatch the non-publishing smoke test from
`main`:

```bash
gh workflow run test-azure-signing.yml --ref main
```

The workflow builds a disposable Windows relay, exchanges GitHub's OIDC token
for Azure access, signs with the configured Artifact Signing certificate
profile, requires an RFC 3161 timestamp, verifies the Authenticode signature and
signer pin, and runs the signed executable. It has no repository or package
write permission and does not upload the executable, create a tag or release,
or promote a container. The successful run URL and signer-certificate SHA-256
in its summary are the integration evidence.

This smoke test exercises the Azure account and certificate profile through a
separate environment identity. The stable workflow's exact
`production-release` federated credential and protected values remain validated
only when an approved stable release runs.
