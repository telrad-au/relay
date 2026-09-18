# Relay image integrity qualification

This Relay-owned harness runs a specified Relay executable over real C-STORE and
verified TLS, then independently reads each landing object and compares exact
received dataset bytes and every decoded frame with generated source images.

By default the HTTPS receiver is a small **test fixture** with real filesystem or
S3 storage. Public Relay CI uses this self-contained adapter and builds the PR's
own Relay revision. For application integration, an operator can select an
independently supplied ingest checkout through `RELAY_INTEGRITY_INGEST_SOURCE`;
that mode uses its actual router, writer and storage implementation. Private
application code is neither copied into this repository nor fetched by public CI.
Evidence explicitly identifies `ingestAdapter: reference` or `application`.

Two explicit modes prevent local results being mistaken for AWS qualification:

| Mode    | Durable store                              | Independent read-back                 | AWS credentials                      |
| ------- | ------------------------------------------ | ------------------------------------- | ------------------------------------ |
| `local` | Temporary filesystem, selected writer      | File read                             | None                                 |
| `aws`   | Real S3, selected writer’s botocore client | Separate botocore client, `GetObject` | Operator's standard credential chain |

Both modes run the same image cases and receipt checks. There is no fake S3 client
or fallback to local storage in AWS mode. A missing binary, invalid account,
unavailable service, missing dependency or failed comparison fails the command;
the integration runner never skips tests. It does not load the application's `.env`.

## What is checked

- Explicit and implicit VR little-endian 16-bit images, multiframe grayscale,
  multiframe RGB, RLE Lossless multiframe and an 8 MiB pixel object that requires
  fragmented network transport. All images are generated synthetic Secondary
  Capture objects, with Unicode, private data elements and nested sequences.
- Original dataset bytes equal the sender's encoded DIMSE dataset and the stored
  dataset. Only Part 10 file meta is excluded from this byte comparison. No
  normalization, tag filtering or dataset reserialization is used on read-back.
- File meta SOP Class UID, SOP Instance UID and Transfer Syntax UID match the
  source. The full stored object's length and SHA-256 match the ingest arrival.
- Pydicom decodes every received frame outside Relay and the ingest service;
  every pixel is compared to the original generated array. Frames have distinct
  pixel patterns. The comparator has negative tests for pixel, metadata, private
  tag, sequence, header, missing-frame and storage-byte changes.
- Identical repeats and different pixels with the same SOP Instance UID create
  distinct arrivals, each independently verified. Expected arrivals reconcile.
- C-STORE success waits for receipt completion. Injected storage failure and a
  mismatching storage checksum must return a DICOM failure without completing or
  registering the arrival. These faults are synthetic injections around the
  selected writer, not deliberate corruption of a real clinical store.

The JSON evidence includes the Relay executable SHA-256, Relay repository Git
revision and dirty state, adapter, storage mode, run UUID, per-case result, dataset/object hashes,
per-frame hashes, arrival counts, timestamps, limitations and AWS cleanup outcome.
Only generated synthetic data is used. Raw images, tokens and patient fields are
not copied into evidence. Hashes here refer to synthetic fixtures only, not a
change to production logging policy.

## Run locally

Prerequisites: Linux or a disposable Windows host, Python 3.12 or later, OpenSSL,
Git, and a Relay executable supporting configuration schema 5. The binary runs
with a temporary configuration;
no Relay installation, enrollment or production service changes are performed.
On Windows, run as Administrator on a disposable host. The harness temporarily
imports its generated public TLS certificate into the machine Root store (the
current-user store prompts interactively) and removes that exact certificate in `finally`,
including on comparison failures. Cleanup failures fail qualification. Use a
disposable host: forcibly killing the process can prevent certificate cleanup.
Linux uses `SSL_CERT_FILE` without changing the system trust store.

From the Relay repository:

```bash
python3 -m venv /tmp/relay-integrity-venv
/tmp/relay-integrity-venv/bin/pip install --require-hashes \
  -r tools/relay-integrity/requirements.lock
cd tools/relay-integrity
/tmp/relay-integrity-venv/bin/python -m pytest integration_tests -q
/tmp/relay-integrity-venv/bin/python -m integration_tests.relay_image_integrity \
  --relay-binary /absolute/path/to/telrad-relay \
  --storage local --evidence /tmp/relay-integrity-local.json
```

Use the exact release executable to qualify a release. A source build tests that
build only; it does not attest to an installed clinic executable. CI builds the
current PR checkout using Go 1.27.0 on both Ubuntu and Windows, runs the
comparator/SDK contract tests and all eleven local integration cases with each
native executable, and retains separate JSON evidence with OS/architecture.
CI uses Python 3.12.13 on Linux and 3.13.15 on Windows (3.12.13 has no
Windows build in the Actions Python distribution).
Windows also verifies certificate removal on success and failure. No AWS
credentials are required. These jobs do not qualify installed services or the
packaged Docker image.

On a disposable Windows host, run from Administrator PowerShell (with OpenSSL on `PATH`):

```powershell
python -m venv "$env:TEMP/relay-integrity-venv"
$python = "$env:TEMP/relay-integrity-venv/Scripts/python.exe"
& $python -m pip install --require-hashes -r tools/relay-integrity/requirements.lock
go build -o "$env:TEMP/telrad-relay.exe" ./cmd/telrad-relay
Set-Location tools/relay-integrity
& $python -m pytest integration_tests -q
& $python -m integration_tests.relay_image_integrity `
  --relay-binary "$env:TEMP/telrad-relay.exe" `
  --storage local --evidence "$env:TEMP/relay-integrity-windows.json"
```

The reference receiver flushes file contents on both platforms and additionally
flushes the parent directory on Linux. Local read-back is not a power-loss test.
The same Windows command supports the explicit AWS arguments below; automatic
CI covers local storage, while live S3 qualification remains operator-invoked.

All Python dependencies are test-only. `requirements.in` and its hash-locked
`requirements.lock` are separate from the distributed Go executable. Regenerate
with `pip-compile --generate-hashes --strip-extras --no-emit-index-url
--no-emit-trusted-host --output-file tools/relay-integrity/requirements.lock
tools/relay-integrity/requirements.in`.

## Use the application's actual ingest implementation

Install the independently supplied application's receipt-ingest development
requirements into the test virtual environment. Then set the source directory:

```bash
RELAY_INTEGRITY_INGEST_SOURCE=/absolute/path/to/application/apps/dicom-receipt-ingest \
/tmp/relay-integrity-venv/bin/python -m integration_tests.relay_image_integrity \
  --relay-binary /absolute/path/to/telrad-relay \
  --storage local --evidence /tmp/relay-integrity-application.json
```

The directory must contain `ingest_service/relay_ingest.py`. Invalid paths or
incompatible dependencies fail rather than falling back to the reference
receiver. Evidence adds the application's Git revision and dirty state. The
adapter uses the existing router, receipt writer and landing-storage APIs; it
requires no application source changes. Authentication, control and receipt
metadata remain in-memory fixtures. Set the same variable with `--storage aws`
to qualify that implementation's real S3 upload and independent SDK read-back.

## Run against AWS S3

Use an explicitly designated **non-production test bucket** without clinical
notifications or consumers. The command does not provision infrastructure or
discover buckets. The operator supplies a bucket, its owning AWS account, region
and optionally a KMS key. The credential account and bucket owner must both match
the expected account. Configured S3 endpoint overrides are rejected; AWS mode
must reach AWS, not an emulator. Leave `DICOM_PACS_ROUTING_ENABLED` unset, because
this isolated harness does not use the application database to resolve PACS routes.

After installing dependencies as above, from `tools/relay-integrity`:

```bash
AWS_PROFILE=your-test-profile \
/tmp/relay-integrity-venv/bin/python -m integration_tests.relay_image_integrity \
  --relay-binary /absolute/path/to/telrad-relay \
  --storage aws \
  --s3-bucket YOUR_DEDICATED_TEST_BUCKET \
  --expected-aws-account YOUR_12_DIGIT_TEST_ACCOUNT \
  --region ap-southeast-2 \
  --evidence /tmp/relay-integrity-aws.json
```

Add `--kms-key-id YOUR_TEST_KMS_KEY_ARN` to exercise SSE-KMS. Use short-lived role
credentials or the configured AWS credential chain. No AWS credentials are passed
to Relay; only the selected storage client and independent reader use them.

Required access is `sts:GetCallerIdentity`, bucket `s3:ListBucket` (for HeadBucket),
and `s3:PutObject`, `s3:GetObject`, `s3:DeleteObject` on
`arn:aws:s3:::YOUR_DEDICATED_TEST_BUCKET/relay-integrity/*`. Versioned buckets also
need `s3:DeleteObjectVersion`; SSE-KMS needs appropriate `kms:GenerateDataKey` and
`kms:Decrypt` grants. Keep this permission separate from clinical buckets.

Each run writes under `relay-integrity/<random-run-uuid>/`. The harness tracks keys
before attempting PUT so an ambiguous failed upload is still cleaned up. On exit,
it deletes only those exact keys (the current object version for versioned
buckets), never a bucket or unrelated prefix. Cleanup errors fail the run and
leave its prefix in evidence for recovery. Give this dedicated prefix a short
lifecycle expiration, including noncurrent versions, to cover host termination
or forced kills where process cleanup cannot execute. S3/KMS requests incur normal
AWS charges. No AWS run is launched automatically from pull requests.

## Interpreting the result

Require exit code zero and `status: passed`. An AWS qualification additionally
requires `storage: aws`, `cleanup: passed`, nine completed arrivals, eleven
attempted arrivals and all eleven case results passing. A local PASS or SDK
contract test using botocore Stubber is **not** a live AWS result.

This establishes C-STORE push integrity through durable landing for the recorded
binary/build, selected receiver and fixtures. Authentication, control sessions and receipt metadata
are in-memory test fixtures. The real application database, SQS recovery,
deployed clinic PACS, C-GET retrieval, HealthImaging conversion and viewer delivery
are not covered. It is not a full DICOM IOD conformance certification or an
exhaustive codec matrix; Relay's existing Orthanc/dcm4che interoperability suite
provides complementary JPEG Lossless coverage. Deployed-path qualification must
also reconcile original/received studies and compare downstream decoded pixels
and clinical metadata before asserting end-to-end clinical preservation.
