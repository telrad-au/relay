"""Run real Relay -> test/application HTTPS ingest -> local/S3 qualification.

By default the HTTPS receiver is a test fixture. Set RELAY_INTEGRITY_INGEST_SOURCE
to qualify an external application ingest checkout. No installed Relay is changed.
"""

import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from copy import deepcopy
from dataclasses import replace
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import platform
import re
import signal
import socket
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlsplit
from uuid import uuid4

from botocore.config import Config
from botocore.exceptions import ClientError
from botocore.session import Session
from fastapi import FastAPI, Response
from pynetdicom import AE, evt
import uvicorn

from .tls_trust import receiver_trust

from .backend import (
    AppConfig,
    S3LandingConfig,
    ScratchConfig,
    StorageConfig,
    LandingStorage,
    ReceiptWriter,
    ReceiptWriteError,
    RelayIngestRuntime,
    build_relay_ingest_router,
    INGEST_SOURCE,
)

from .image_integrity import (
    ImageCase,
    IntegrityFailure,
    digest,
    fixtures,
    require,
    verify,
)


CREDENTIAL = "trr_v1_" + "A" * 22 + "_" + "B" * 43  # Synthetic, local fixture only.
LIMITATIONS = [
    "Authentication, control sessions and receipt metadata use in-memory fixtures.",
    "No real application database, SQS recovery, HealthImaging import or viewer is exercised.",
    "C-STORE push is exercised; PACS C-GET retrieval and installed clinic releases are not.",
    "Synthetic uncompressed/RLE coverage is not qualification of every modality or codec.",
]


class ReceiptAPI:
    """Metadata/control fixture shared by both receiver adapters."""

    def __init__(self):
        self.arrivals = {}
        self.receipts = {}
        self.completed = set()
        self.hold_completion = False
        self.completion_entered = threading.Event()
        self.release_completion = threading.Event()

    def authenticate_relay(self, *, authorization, protocol):
        require(
            authorization == f"Bearer {CREDENTIAL}" and protocol == "dicom",
            "invalid fixture auth",
        )
        return {
            "relayId": "integrity-relay",
            "siteId": "integrity-relay",
            "companyId": "synthetic-integrity",
            "ingestMode": "TEST",
        }

    def create_relay_dicom_arrival(self, **kwargs):
        receipt = str(uuid4())
        self.arrivals[receipt] = kwargs
        return {
            "receiptId": receipt,
            "receiptCreatedAt": datetime.now(timezone.utc).isoformat(),
        }

    def record_receipt(self, payload):
        self.receipts[payload["id"]] = payload
        return {"ok": True, "receipt": {"id": payload["id"]}}

    def complete_relay_dicom_receipt(self, *, receipt_id, landing_receipt_id):
        require(
            receipt_id == landing_receipt_id and receipt_id in self.receipts,
            "completion without landing receipt",
        )
        if self.hold_completion:
            self.completion_entered.set()
            require(self.release_completion.wait(10), "completion gate timed out")
        self.completed.add(receipt_id)
        return {"ok": True}


class TrackedStorage(LandingStorage):
    def __init__(self, config):
        super().__init__(config)
        self.attempted_keys = set()

    def put_s3(self, file_path, *, context, received_at):
        # Track before PUT, including ambiguous network failures, for exact cleanup.
        self.attempted_keys.add(
            self._s3_key(context.tenant_id, context.receipt_id, received_at)
        )
        return super().put_s3(file_path, context=context, received_at=received_at)


class FaultWriter(ReceiptWriter):
    fault = None

    def receive_file(self, **kwargs):
        if self.fault == "storage-failure":
            raise ReceiptWriteError(
                "synthetic storage failure", durability_failure=True
            )
        stored = super().receive_file(**kwargs)
        if self.fault == "checksum-mismatch":
            return replace(stored, checksum_sha256="0" * 64)
        return stored


def bound_socket():
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    return sock


def free_port():
    with bound_socket() as sock:
        return sock.getsockname()[1]


@contextmanager
def cloud_server(directory, config, api, writer):
    cert, key = directory / "cert.pem", directory / "key.pem"
    subprocess.run(
        [
            "openssl",
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            str(key),
            "-out",
            str(cert),
            "-days",
            "1",
            "-subj",
            "/CN=localhost",
            "-addext",
            "subjectAltName=IP:127.0.0.1",
        ],
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        timeout=15,
    )
    key.chmod(0o600)
    app = FastAPI()
    app.include_router(
        build_relay_ingest_router(
            config,
            RelayIngestRuntime(5),
            api_client=api,
            writer=writer,
        )
    )
    with bound_socket() as sock:
        origin = f"https://127.0.0.1:{sock.getsockname()[1]}"

        @app.post("/v1/relay/control/sessions", status_code=201)
        def session():
            return {
                "type": "ready",
                "sessionId": "integrity-session",
                "connectorId": "integrity-relay",
                "ingestMode": "TEST",
                "transports": {
                    name: {
                        "url": f"{origin}/v1/relay/ingest/{name}",
                        "contentType": content,
                    }
                    for name, content in (
                        ("dicom", "application/dicom"),
                        ("hl7", "application/hl7-v2"),
                    )
                },
            }

        @app.post("/v1/relay/control/sessions/integrity-session/poll")
        @app.delete("/v1/relay/control/sessions/integrity-session")
        def idle():
            return Response(status_code=204)

        server = uvicorn.Server(
            uvicorn.Config(
                app,
                ssl_keyfile=str(key),
                ssl_certfile=str(cert),
                access_log=False,
                log_level="critical",
                timeout_graceful_shutdown=5,
            )
        )
        thread = threading.Thread(
            target=server.run, kwargs={"sockets": [sock]}, daemon=True
        )
        thread.start()
        try:
            deadline = time.monotonic() + 10
            while not server.started:
                require(
                    thread.is_alive() and time.monotonic() < deadline,
                    "TLS ingest did not start",
                )
                time.sleep(0.05)
            yield origin, cert
        finally:
            api.release_completion.set()
            server.should_exit = True
            thread.join(10)
            require(not thread.is_alive(), "TLS ingest did not stop")


@contextmanager
def relay_process(binary, directory, origin, cert):
    dicom_port, hl7_port = free_port(), free_port()
    while hl7_port == dicom_port:
        hl7_port = free_port()
    credential = directory / "relay-credential.json"
    credential.write_text(json.dumps({"schemaVersion": 1, "credential": CREDENTIAL}))
    credential.chmod(0o600)
    config = directory / "relay.json"
    config.write_text(
        json.dumps(
            {
                "schemaVersion": 5,
                "pairingUrl": f"{origin}/v1/relay/pairing-enrollments",
                "controlUrl": f"{origin}/v1/relay/control",
                "dicomUrl": f"{origin}/v1/relay/ingest/dicom",
                "hl7Url": f"{origin}/v1/relay/ingest/hl7",
                "relayId": "integrity-relay",
                "credentialPath": credential.name,
                "listenAddress": "127.0.0.1",
                "dicomPort": dicom_port,
                "hl7Port": hl7_port,
                "reportHost": "127.0.0.1",
                "reportPort": free_port(),
            }
        )
    )
    config.chmod(0o600)
    # Do not pass AWS credentials or endpoint overrides to the Relay subprocess.
    environment = {
        key: value for key, value in os.environ.items() if not key.startswith("AWS_")
    }
    environment["SSL_CERT_FILE"] = str(cert)
    process = subprocess.Popen(
        [str(binary), "--config", str(config), "run"],
        env=environment,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        deadline = time.monotonic() + 15
        while True:
            require(process.poll() is None, "Relay exited before listening")
            try:
                with socket.create_connection(("127.0.0.1", dicom_port), timeout=0.2):
                    break
            except OSError:
                require(time.monotonic() < deadline, "Relay listener timed out")
                time.sleep(0.05)
        yield dicom_port
    finally:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def send(case, port):
    sent = []

    def capture(event):
        if event.message.command_set.CommandField == 0x0001:
            sent.append(event.message.data_set.getvalue())

    ae = AE(ae_title="INTEGRITY_SCU")
    ae.acse_timeout = 10
    ae.dimse_timeout = 45
    ae.network_timeout = 45
    ae.add_requested_context(
        case.dataset.SOPClassUID, case.dataset.file_meta.TransferSyntaxUID
    )
    association = ae.associate(
        "127.0.0.1",
        port,
        ae_title="INTEGRITY_SCP",
        evt_handlers=[(evt.EVT_DIMSE_SENT, capture)],
    )
    require(association.is_established, f"{case.name}: association rejected")
    try:
        status = association.send_c_store(case.dataset)
        require(len(sent) == 1, f"{case.name}: missing sent dataset capture")
        return getattr(status, "Status", None), sent[0]
    finally:
        if association.is_established:
            association.release()
        else:
            association.abort()


def aws_reader(args, storage):
    require(
        args.s3_bucket and re.fullmatch(r"\d{12}", args.expected_aws_account or ""),
        "AWS mode requires a dedicated test bucket and expected 12-digit AWS account",
    )
    session = Session()
    cfg = Config(
        connect_timeout=10,
        read_timeout=30,
        retries={"max_attempts": 2},
        ignore_configured_endpoint_urls=True,
    )
    sts = session.create_client("sts", region_name=args.region, config=cfg)
    try:
        require(
            sts.get_caller_identity()["Account"] == args.expected_aws_account,
            "AWS account mismatch",
        )
    finally:
        sts.close()
    reader = session.create_client("s3", region_name=args.region, config=cfg)
    try:
        reader.head_bucket(
            Bucket=args.s3_bucket, ExpectedBucketOwner=args.expected_aws_account
        )
        # The selected writer resolves its own SDK client. Reject emulators and
        # configured endpoint overrides instead of labelling their result AWS.
        require(
            storage._s3().meta.endpoint_url == reader.meta.endpoint_url,
            "S3 writer has an endpoint override",
        )
        return reader
    except BaseException:
        reader.close()
        raise


def read_landed(location, args, reader, storage):
    if args.storage == "local":
        return Path(location).read_bytes()
    uri = urlsplit(location)
    key = uri.path.lstrip("/")
    require(
        uri.scheme == "s3"
        and uri.netloc == args.s3_bucket
        and key in storage.attempted_keys,
        "unexpected landing object location",
    )
    response = reader.get_object(
        Bucket=args.s3_bucket,
        Key=key,
        ExpectedBucketOwner=args.expected_aws_account,
        ChecksumMode="ENABLED",
    )
    with response["Body"] as body:
        payload = body.read()
    require(len(payload) == response["ContentLength"], "S3 read-back length mismatch")
    require(
        response.get("ChecksumSHA256")
        == base64.b64encode(bytes.fromhex(digest(payload))).decode("ascii"),
        "S3 read-back checksum missing or mismatched",
    )
    return payload


def cleanup_s3(args, reader, storage):
    failures = 0
    for key in sorted(storage.attempted_keys):
        try:
            require(
                key.startswith(storage.config.s3_landing.prefix + "/"),
                "cleanup key outside run",
            )
            request = {
                "Bucket": args.s3_bucket,
                "Key": key,
                "ExpectedBucketOwner": args.expected_aws_account,
            }
            metadata = reader.head_object(**request)
            # Remove the exact version, not a delete marker, in versioned buckets.
            if "VersionId" in metadata:
                request["VersionId"] = metadata["VersionId"]
            reader.delete_object(**request)
        except ClientError as exc:
            if exc.response.get("ResponseMetadata", {}).get("HTTPStatusCode") != 404:
                failures += 1
        except Exception:
            failures += 1
    require(failures == 0, "S3 cleanup failed; inspect this run's isolated prefix")


def exercise(port, api, writer, storage, reader, args, report):
    cases = fixtures()
    cases.append(ImageCase("identical-repeat", cases[0].dataset, cases[0].pixels))
    changed = deepcopy(cases[0].dataset)
    pixels = cases[0].pixels.copy()
    pixels.flat[0] ^= 1
    changed.PixelData = pixels.tobytes()
    cases.append(ImageCase("same-uid-different-pixels", changed, pixels))
    for case in cases:
        before = set(api.completed)
        code, wire = send(case, port)
        require(code == 0, f"{case.name}: C-STORE did not succeed")
        arrivals = api.completed - before
        require(
            len(arrivals) == 1, f"{case.name}: expected exactly one completed arrival"
        )
        receipt = arrivals.pop()
        landed = read_landed(api.receipts[receipt]["filePath"], args, reader, storage)
        incoming = api.arrivals[receipt]
        require(
            len(landed) == incoming["payload_size_bytes"],
            f"{case.name}: arrival length mismatch",
        )
        report["cases"].append(verify(case, wire, landed, incoming["payload_sha256"]))

    # Hold completion after storage: C-STORE must wait for the durable receipt.
    api.hold_completion = True
    before = set(api.completed)
    with ThreadPoolExecutor(max_workers=1) as executor:
        future = executor.submit(send, cases[0], port)
        try:
            require(api.completion_entered.wait(10), "completion gate was not reached")
            time.sleep(0.2)
            require(not future.done(), "C-STORE succeeded before receipt completion")
        finally:
            api.release_completion.set()
        code, wire = future.result(timeout=45)
    api.hold_completion = False
    require(
        code == 0 and len(api.completed - before) == 1,
        "delayed receipt did not succeed",
    )
    receipt = (api.completed - before).pop()
    landed = read_landed(api.receipts[receipt]["filePath"], args, reader, storage)
    evidence = verify(cases[0], wire, landed, api.arrivals[receipt]["payload_sha256"])
    evidence["case"] = "waits-for-receipt"
    report["cases"].append(evidence)

    for fault in ("storage-failure", "checksum-mismatch"):
        writer.fault = fault
        completed, registered = set(api.completed), set(api.receipts)
        code, _ = send(cases[0], port)
        require(
            code is not None and code != 0, f"{fault}: expected C-STORE failure status"
        )
        require(
            api.completed == completed and set(api.receipts) == registered,
            f"{fault}: corrupt/failed landing was acknowledged",
        )
        report["cases"].append({"case": fault, "status": "passed", "dicomStatus": code})
    writer.fault = None
    require(
        len(api.completed) == len(cases) + 1 and len(api.arrivals) == len(cases) + 3,
        "arrival inventory mismatch",
    )
    report["completedArrivals"] = len(api.completed)
    report["attemptedArrivals"] = len(api.arrivals)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--relay-binary", required=True, type=Path)
    parser.add_argument("--storage", choices=("local", "aws"), required=True)
    parser.add_argument("--s3-bucket")
    parser.add_argument("--expected-aws-account")
    parser.add_argument("--region", default="ap-southeast-2")
    parser.add_argument("--kms-key-id", default="")
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()

    def interrupted(_signal, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    run = str(uuid4())
    report = {
        "schemaVersion": 1,
        "runId": run,
        "status": "failed",
        "storage": args.storage,
        "ingestAdapter": "application" if INGEST_SOURCE else "reference",
        "platform": {"system": platform.system(), "machine": platform.machine()},
        "startedAt": datetime.now(timezone.utc).isoformat(),
        "cases": [],
        "limitations": LIMITATIONS,
    }
    reader, storage = None, None
    try:
        require(
            os.environ.get("DICOM_PACS_ROUTING_ENABLED", "false").lower() != "true",
            "unset DICOM_PACS_ROUTING_ENABLED for the isolated integrity harness",
        )
        args.relay_binary = args.relay_binary.resolve(strict=True)
        require(os.access(args.relay_binary, os.X_OK), "Relay binary is not executable")
        report["relayBinarySha256"] = digest(args.relay_binary.read_bytes())
        if INGEST_SOURCE:
            report["applicationRevision"] = subprocess.check_output(
                ["git", "-C", str(INGEST_SOURCE), "rev-parse", "HEAD"],
                text=True,
                timeout=10,
            ).strip()
            report["applicationDirty"] = bool(
                subprocess.check_output(
                    ["git", "-C", str(INGEST_SOURCE), "status", "--porcelain"],
                    text=True,
                    timeout=10,
                ).strip()
            )
        else:
            report["limitations"] = [
                "HTTPS ingest uses a test receiver, not the application implementation."
            ] + LIMITATIONS
        root = Path(__file__).resolve().parents[3]
        report["relayRepositoryRevision"] = subprocess.check_output(
            ["git", "-C", str(root), "rev-parse", "HEAD"], text=True, timeout=10
        ).strip()
        report["relayRepositoryDirty"] = bool(
            subprocess.check_output(
                ["git", "-C", str(root), "status", "--porcelain"], text=True, timeout=10
            ).strip()
        )
        with tempfile.TemporaryDirectory(prefix="relay-integrity-") as temporary:
            directory = Path(temporary)
            config = AppConfig(
                storage=StorageConfig(data_dir=directory / "landing"),
                scratch=ScratchConfig(min_free_bytes=0, max_reserved_bytes=2 * 1024**3),
                landing_storage_backend="s3" if args.storage == "aws" else "local",
                s3_landing=S3LandingConfig(
                    region=args.region,
                    bucket=args.s3_bucket or "",
                    prefix=f"relay-integrity/{run}",
                    kms_key_id=args.kms_key_id,
                    timeout_seconds=15,
                ),
            )
            storage = TrackedStorage(config)
            if args.storage == "aws":
                report["s3Prefix"] = config.s3_landing.prefix
                report["awsRegion"] = args.region
                reader = aws_reader(args, storage)
            api = ReceiptAPI()
            writer = FaultWriter(config, api, storage)
            with cloud_server(directory, config, api, writer) as (origin, cert):
                with receiver_trust(cert), relay_process(
                    args.relay_binary, directory, origin, cert
                ) as port:
                    exercise(port, api, writer, storage, reader, args, report)
        report["status"] = "passed"
    except Exception as exc:
        report["error"] = (
            str(exc) if isinstance(exc, IntegrityFailure) else type(exc).__name__
        )
    except KeyboardInterrupt:
        report["error"] = "interrupted"
    finally:
        if reader is not None:
            try:
                cleanup_s3(args, reader, storage)
                report["cleanup"] = "passed"
            except Exception as exc:
                report["status"] = "failed"
                report["cleanup"] = "failed"
                report["cleanupError"] = type(exc).__name__
            finally:
                reader.close()
        report["finishedAt"] = datetime.now(timezone.utc).isoformat()
        args.evidence.parent.mkdir(parents=True, exist_ok=True)
        args.evidence.write_text(json.dumps(report, indent=2) + "\n")
    print(
        f"Relay image integrity ({args.storage}): {report['status']}; evidence: {args.evidence}"
    )
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
