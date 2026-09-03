# Agent Notes

## Source of Truth

Before changing behavior, inspect the relevant implementation and its tests, plus:

- `README.md` for the public product contract.
- `docs/testing.md` for required validation.
- `docs/releases.md` for release and promotion policy.
- `docs/native-operations.md` and `docs/container-operations.md` for operator behavior.
- `packaging/relay.example.json` for the configuration contract.

Do not replace current source-of-truth behavior with assumptions from old releases,
issues, or prior runs.

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
- Installer, manifest, workflow, and distributed-file changes must update the
  corresponding contract or generated-bundle tests.
- Prefer protocol-real fixtures and semantics over fixtures tailored to the
  current parser.

## Compatibility Surfaces

Before changing any of these, identify the compatibility and migration impact:

- public `telrad` commands and output;
- configuration schema and migration behavior;
- environment variables and packaged defaults;
- pairing, control, DICOM, and HL7 endpoint contracts;
- DICOM association, DIMSE, and status behavior;
- HL7 framing, acknowledgement, retry, and idempotency behavior;
- Linux and Windows service installation and upgrades;
- container configuration and persistent storage; and
- update manifests, signatures, trust roots, installers, and release assets.

Keep documentation, examples, installers, workflows, and tests synchronized with
contract changes.

## Protocol and Privacy Invariants

- Relay must not persist clinical ingest payloads or write payloads, credentials,
  authorization values, DICOM UIDs, or HL7 control IDs to operational logs.
- DICOM C-STORE success requires a valid cloud receipt. Do not add hidden retries,
  spooling, transcoding, or deduplication without an explicit design change.
- For DIMSE `CommandDataSetType`, only `0x0101` means no dataset; standard values
  such as `0x0001` are dataset-present.
- C-ECHO does not prove C-STORE interoperability. Test C-STORE with realistic
  Orthanc/DCMTK behavior.
- HL7 application success requires the exact correlated application ACK. Do not
  treat enhanced commit acknowledgements as application success without a
  deliberate state-machine design.
- Authenticated Relay requests must not follow redirects. Update downloads may
  follow redirects only because their artifacts and metadata are independently
  signature-verified.

## Releases and Updates

- Building or testing release artifacts does not authorize publication.
- Testing, development, and stable releases use separate trust material and
  channels.
- Testing releases must not advance stable or `latest`.
- Never reuse or move an immutable release version or tag.
- `telrad update` checks only. Applying an update requires explicit approval of
  the exact signed version.
- Treat push, merge, tag, release publication, container promotion, deployment,
  and visibility changes as separate authorization boundaries.
