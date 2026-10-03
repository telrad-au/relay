# Relay image integrity

This dedicated suite checks the current Relay TCP proxy with synthetic DICOM
images. A real C-STORE sender connects to the actual Relay executable over plain
TCP. Relay wraps that connection in mutual TLS to a loopback DICOM SCP, which
captures each dataset in memory. The comparison checks the received dataset's
exact bytes and every decoded pixel in every frame.

Relay does not parse DICOM, add a Part 10 header, issue a receipt, transcode or
store images. The upstream SCP owns association negotiation and C-STORE status.
This suite follows the accepted [architecture](architecture.md#dicom).

## What is checked

- Exact equality between the source dataset, the sender's captured DIMSE dataset
  and the upstream SCP's received dataset. The received dataset is compared
  before parsing or reserialization; no normalization or tag filtering is allowed.
- Every decoded pixel in every frame matches the generated source array. The
  comparator decodes the raw received dataset using its negotiated transfer
  syntax. Part 10 encoding is used only to prepare source fixtures; the receiver
  does not add a file wrapper.
- Explicit and implicit VR little-endian images, multiframe grayscale and RGB,
  RLE Lossless, and an 8 MiB pixel object requiring fragmented transport.
  Fixtures include Unicode, private elements and nested sequences.
- Identical repeats and changed pixels with the same SOP Instance UID arrive
  separately and retain their exact respective contents.
- Withholding the SCP's C-STORE response prevents the sender from completing;
  the SCP's success and failure statuses reach the sender unchanged.
- The complete plaintext DICOM PDU streams match byte for byte in both
  directions across Relay, including association negotiation, DIMSE commands,
  dataset fragments, C-STORE responses and association release. This checks
  transport preservation independently of dataset decoding.
- The synthetic upstream requires Relay's client certificate, and Relay's
  identity pins the generated CA for the upstream data connection.

Negative comparison tests detect changed pixels, metadata, private tags, nested
sequences, missing frames and damaged datasets. These tests validate the
comparator; they do not imply Relay inspects or rejects DICOM contents.

## Targets and evidence

`.github/workflows/image-integrity.yml` builds the PR revision's native Linux
and Windows binaries and its actual release Dockerfile image. Each target runs
the same ten C-STORE cases plus fixture and protocol checks and writes separate
JSON evidence. The
native targets start a process with temporary state; they do not install or
qualify an operating-system service.

Schema-3 evidence records the target platform, binary hash, repository revision
and working-tree state, image ID where applicable, case results and coverage
limitations. Each case includes dataset byte count and SHA-256, decoded frame
hashes, forward/reverse stream byte counts and SHA-256, DICOM status, mutual-TLS
verification and the authenticated client certificate SHA-256. Totals distinguish
attempted arrivals, successful stores and rejected stores. Review the evidence from the actual revision
being qualified; the existence of this workflow is not evidence that a target
has passed. CI retains only the synthetic evidence report, not datasets,
certificate keys or the temporary identity.

## Run locally

Prerequisites: Python 3.12 or later, OpenSSL, Git and a Relay binary supporting
configuration schema 6. Build with the Go version pinned in CI (currently 1.27.0).
All image payloads, certificate authorities, server certificates and EC client
identities are generated at runtime. The harness seeds a version-1
`identity.json` containing the client certificate, key and pinned CA, bypassing
pairing. Its temporary `relay.json` sets `dataDir` to `.`, which Relay resolves
relative to the configuration file.

The harness does not modify any system certificate store. The generated CA is
trusted only by the test endpoints and the temporary Relay identity; no
administrator privileges are needed for a native process run.

```bash
python3 -m venv /tmp/relay-integrity-venv
/tmp/relay-integrity-venv/bin/pip install --require-hashes \
  -r tools/relay-integrity/requirements.lock
go build -o /tmp/telrad-relay ./cmd/telrad-relay
cd tools/relay-integrity
/tmp/relay-integrity-venv/bin/python -m pytest integration_tests -q
/tmp/relay-integrity-venv/bin/python -m integration_tests.relay_image_integrity \
  --relay-binary /tmp/telrad-relay --evidence /tmp/relay-integrity.json
```

On Windows, run from PowerShell with OpenSSL on `PATH` (CI uses the Git Bash
runner environment). Elevation and certificate-store installation are unnecessary.

```powershell
python -m venv "$env:TEMP/relay-integrity-venv"
$python = "$env:TEMP/relay-integrity-venv/Scripts/python.exe"
& $python -m pip install --require-hashes -r tools/relay-integrity/requirements.lock
go build -o "$env:TEMP/telrad-relay.exe" ./cmd/telrad-relay
Set-Location tools/relay-integrity
& $python -m pytest integration_tests -q
& $python -m integration_tests.relay_image_integrity `
  --relay-binary "$env:TEMP/telrad-relay.exe" `
  --evidence "$env:TEMP/relay-integrity-windows.json"
```

For Docker, use a disposable Linux Docker host with root or noninteractive sudo
available to set ownership of the temporary state directory:

```bash
docker build -t relay-integrity:current .
cd tools/relay-integrity
/tmp/relay-integrity-venv/bin/python -m integration_tests.relay_image_integrity \
  --relay-image relay-integrity:current --evidence /tmp/relay-integrity-docker.json
```

The image runs by its inspected image ID with its packaged entrypoint and user
`10001:10001`, host networking, a read-only root filesystem, all capabilities
dropped and no new privileges. Only `relay.json` and `identity.json` are copied
into its private state mount. The CA signing key and upstream server key remain
outside the container. The container is removed and state ownership restored
before the temporary directory is deleted, including after comparison failures.

Dependencies are test-only and hash-locked. The HTTP fixture and its FastAPI and
Uvicorn dependencies are no longer used. Regenerate the lock with:

```bash
pip-compile --generate-hashes --strip-extras --no-emit-index-url \
  --no-emit-trusted-host --output-file tools/relay-integrity/requirements.lock \
  tools/relay-integrity/requirements.in
```

## Interpreting results

Require exit code zero, `schemaVersion: 3`, `status: passed`, all ten cases
passing, `attemptedArrivals: 10`, `successfulStores: 9` and `rejectedStores: 1`.
The intentional upstream failure case still passes when the exact upstream
failure status reaches the sender. Dataset, frame and bidirectional stream
comparisons must all pass, and each case must record verified mutual TLS. Docker
evidence must additionally record successful container cleanup. Missing prerequisites,
failed comparisons and unavailable required targets fail their run; the
integration runner does not silently skip cases. The fixture unit tests, native
process runs and packaged-image run are distinct validation stages, so report
which stages actually ran.

A pass establishes preservation for the recorded executable and fixtures. It
does not qualify every modality or codec, installed Linux or Windows services,
installers, pairing, certificate renewal, HL7/report processing, or production
Telrad DICOM listeners and downstream storage. The upstream here is a synthetic
mutual-TLS DICOM SCP. Deployment qualification remains separate from this suite.
