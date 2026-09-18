# Relay image integrity

This suite verifies that Relay preserves the original DICOM dataset byte for byte,
allowing only the added Part 10 file header. It runs the actual Relay executable
through real C-STORE and verified HTTPS. A loopback test receiver captures uploads
in memory and validates them before returning a synthetic receipt. No image is
saved to disk or cloud storage by the receiver.

## What is checked

- Exact equality between the source dataset, the sender's captured DIMSE dataset,
  and the uploaded dataset. No normalization, tag filtering or reserialization
  of the received dataset is used for this comparison.
- The added Part 10 header has the correct SOP Class UID, SOP Instance UID and
  Transfer Syntax UID. Header differences are permitted; dataset differences fail.
- Every decoded pixel in every frame matches the generated source array.
- Explicit and implicit VR little-endian images, multiframe grayscale and RGB,
  RLE Lossless, and an 8 MiB pixel object requiring fragmented transport.
  Fixtures include Unicode, private elements and nested sequences.
- Identical repeats and changed pixels with the same SOP Instance UID arrive
  separately and retain their exact respective contents.
- C-STORE waits for the receiver's receipt, and receiver rejection produces a
  failure status. These are protocol fixtures, not tests of production storage.

Negative tests prove that pixel, metadata, private-tag, nested-sequence, frame,
header and payload changes are detected. The receiver also rejects altered
uploads without issuing a receipt.

CI builds the PR revision on native Linux and Windows and builds the actual
release Dockerfile image. Each target runs the same ten integration cases and
retains separate JSON evidence. Evidence includes platform, binary hash,
repository revision/dirty state, image ID where applicable, case results,
dataset/object/frame hashes, arrival counts, timestamps and coverage limitations.
Only synthetic data is used; payloads and credentials are not included in evidence.

## Run locally

Prerequisites: Python 3.12 or later, OpenSSL, Git and a Relay binary supporting
configuration schema 5. Build with the Go version pinned in CI (currently 1.27.0).
The harness runs Relay with temporary configuration, without installing a service.

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

On a disposable Windows host, run from Administrator PowerShell with OpenSSL on
`PATH`. The harness temporarily adds its generated TLS certificate to the machine
Root store and removes that exact certificate in `finally`, including on test
failure. Cleanup failures fail the run. Forcibly killing the process can prevent
cleanup. Linux uses `SSL_CERT_FILE` without changing the system trust store.

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

The image runs with its packaged entrypoint and user `10001:10001`, a read-only
root filesystem, all capabilities dropped and no new privileges. Only synthetic
configuration, credentials and the public test certificate enter its state mount.
The container and temporary state are removed afterward.

Dependencies are test-only and hash-locked. Regenerate the lock with
`pip-compile --generate-hashes --strip-extras --no-emit-index-url
--no-emit-trusted-host --output-file tools/relay-integrity/requirements.lock
tools/relay-integrity/requirements.in`.

## Interpreting results

Require exit code zero, `status: passed`, all ten cases passing, nine completed
arrivals and ten attempted arrivals. Docker evidence must also show successful
container cleanup. Missing dependencies or failed comparisons fail the command;
the integration runner never skips cases.

A pass establishes dataset preservation for the recorded executable and covered
fixtures. It does not establish every modality/codec, C-GET retrieval, installed
services, or downstream application processing. Relay's existing Orthanc/dcm4che
interoperability suite provides complementary JPEG Lossless coverage. Application
storage and deployed end-to-end integration belong in separate tests.
