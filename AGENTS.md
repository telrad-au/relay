# Agent Notes

## Engineering Approach

Apply these steps in order, within the requested scope and this file's existing
protocol, privacy, validation, and release constraints:

1. **Question the requirements.** Understand the problem each requirement solves,
   where it came from, and whether it is still necessary. Challenge assumptions.
2. **Delete unnecessary parts or processes.** Remove code, dependencies,
   abstractions, and steps that are not needed to meet the validated requirements.
3. **Simplify and optimize.** Make what remains as simple as possible. Optimize
   only after establishing that it needs to exist.
4. **Accelerate cycle time.** Shorten feedback loops and address measured
   bottlenecks after deleting and simplifying.
5. **Automate last.** Automate the remaining useful, repeatable process once it is
   understood and stable.

## Source of Truth

Before changing behavior, inspect the relevant implementation and its tests, plus:

- `docs/architecture.md` for the accepted design. Changing it is a design
  change, not a documentation edit.
- `README.md` for the public product contract.
- `docs/testing.md` for required validation.
- `docs/releases.md` for release and promotion policy.
- `docs/native-operations.md` and `docs/container-operations.md` for operator behavior.
- `cmd/telrad-relay/config.go` for the configuration contract.

Do not replace current source-of-truth behavior with assumptions from old releases,
issues, or prior runs. Earlier designs (HTTPS ingest, bearer credentials, report
permits, PACS retrieval, gateway mode, self-update) are removed; do not
reintroduce them without a design change.

## Worktree and Change Scope

- Inspect `git status` before editing and preserve unrelated work.
- Keep changes focused and avoid opportunistic refactors.
- Put cohesive new logic in a focused file instead of further growing large
  orchestration files such as `main.go`.
- Stage only reviewed paths. Before committing, inspect the staged diff and run
  `git diff --cached --check`.
- Do not commit, push, merge, tag, publish, deploy, promote, or change visibility
  unless explicitly requested.
- Recheck current GitHub branch rules before choosing a direct push or PR workflow.
  Never force-push.

## Validation

Use the Go version pinned by `.github/workflows/ci.yml`.

While iterating, run focused tests. Before finalizing a Go behavior change, run:

```bash
gofmt -w <changed-go-files>
go test -race ./...
go vet ./...
scripts/check-publication.sh
scripts/check-licenses.sh
```

Run `govulncheck ./...` when dependencies or security-sensitive code change.

Loopback socket and Go cache failures in a managed sandbox can be environment
restrictions. Distinguish those from product failures before changing code.

## Tests

- Add behavioral tests for user-visible or externally observable changes.
- Keep tests beside their owning package in `*_test.go` files.
- Protocol changes require integration-level coverage, not only helper-unit tests.
- Use synthetic identifiers and payloads only. Never add PHI, credentials,
  signing keys, internal tokens, or production certificates to fixtures.
- Installer, workflow, and distributed-file changes must update the
  corresponding contract or generated-bundle tests.
- Prefer protocol-real fixtures and semantics over fixtures tailored to the
  current parser.

## Compatibility Surfaces

Before changing any of these, identify the compatibility impact:

- public `telrad` commands and `status` output;
- the `/status` and `/readyz` endpoints;
- configuration schema, `TELRAD_RELAY_<FIELD>` environment variables, and
  packaged defaults;
- `identity.json`, `accessions.ledger` and `accept-backlog.json` on the data
  volume;
- the enrolment endpoint contract (`X-Telrad-Relay-Protocol`), pairing link and
  token flows, and renewal;
- the TLS connections to Telrad's DICOM, HL7 and report ports, and the report
  pickup exchange;
- HL7 framing, acknowledgement correlation, ledger, refusal, and duplicate
  behavior;
- Linux and Windows service installation and reinstall upgrades;
- container configuration and persistent storage; and
- installers and release assets.

Keep documentation, examples, installers, workflows, and tests synchronized with
contract changes.

## Protocol and Privacy Invariants

- Relay never rewrites forwarded bytes. DICOM is a byte pipe; HL7 frames,
  Telrad's acknowledgements, reports, and the report receiver's acknowledgements
  pass byte for byte. Relay composes only its own `AR` refusal and `AE`
  receiver-failure acknowledgements.
- Relay does not parse DICOM. Do not add hidden retries, spooling, transcoding,
  or deduplication without an explicit design change.
- The ledger is appended and synced to disk before an `AA` for an `NW` or `XO`
  order is forwarded to the RIS. If the append fails, close the connection
  instead of forwarding the `AA`.
- Correlate Telrad's replies to clinic messages by `MSA-2` = `MSH-10`, not by
  arrival order.
- Accession numbers appear only in `accessions.ledger`. Never log them or put
  them in status output or error text. Ledger entries are never removed.
- Relay must not persist clinical payloads or write message bytes, DICOM UIDs,
  HL7 control IDs, patient identifiers, pairing tokens, or key material to logs.
- The outbound report pickup connection is the only report path. Do not add a
  report listener. A report is delivered only when every `OBR-18` is in the
  ledger, or while a clinic-opened `accept-backlog` window is open, in which
  case the missing accessions are appended and synced before delivery.
- Every connection to Telrad is outbound. Connections to the DICOM, HL7 and
  report ports present the Relay's client certificate. Enrolment and renewal
  requests present none; renewal is signed with the current key.
- Enrolment and renewal requests never follow redirects and verify Telrad with
  the operating system trust store. The data ports are verified against the
  Telrad Relay CA from the issued identity only. Relay ships no trust material.
- The status endpoint binds to loopback only and carries nothing clinical or
  secret.

## Releases and Updates

- Building or testing release artifacts does not authorize publication.
- Relay does not update itself. Operators update by rerunning the installer for
  a version or pulling a new image. Do not add self-update, update manifests, or
  release trust roots without a design change.
- Never reuse or move an immutable release version or tag.
- A stable release promotes the tested prerelease container digest without
  rebuilding it; `latest` moves only after the stable release verifies.
- Treat push, merge, tag, release publication, container promotion, deployment,
  and visibility changes as separate authorization boundaries.
