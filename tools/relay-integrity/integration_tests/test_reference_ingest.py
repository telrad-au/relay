"""Exercise the public test receiver's real SDK request construction offline."""

import base64
import os
import subprocess
import sys
from datetime import datetime, timezone
from hashlib import sha256
from pathlib import Path

from botocore.session import Session
from botocore.stub import Stubber

from .reference_ingest import (
    AppConfig,
    LandingStorage,
    ReceiptContext,
    ReceiptWriter,
    S3LandingConfig,
    StorageConfig,
)


def test_local_receiver_flushes_and_reads_exact_bytes(tmp_path, monkeypatch):
    source = tmp_path / "source.dcm"
    payload = b"synthetic image payload"
    source.write_bytes(payload)
    config = AppConfig(storage=StorageConfig(data_dir=tmp_path / "landing"))
    writer = ReceiptWriter(config, None, LandingStorage(config))
    original_fsync = os.fsync
    flushed = []

    def fsync(fd):
        original_fsync(fd)
        flushed.append(fd)

    monkeypatch.setattr(os, "fsync", fsync)
    stored = writer.receive_file(
        source_path=source,
        receipt_id="one",
        tenant_id="synthetic",
        received_at=datetime.now(timezone.utc),
    )
    assert len(flushed) == (1 if os.name == "nt" else 2)
    assert Path(stored.landing_location).read_bytes() == payload
    assert stored.byte_size == len(payload)
    assert stored.checksum_sha256 == sha256(payload).hexdigest()


def test_reference_receiver_puts_exact_bytes_with_service_checksum(tmp_path):
    payload = b"synthetic image payload"
    source = tmp_path / "synthetic.dcm"
    source.write_bytes(payload)
    config = AppConfig(
        s3_landing=S3LandingConfig(
            bucket="synthetic-test-bucket", prefix="relay-integrity/run"
        )
    )
    storage = LandingStorage(config)
    storage.client = Session().create_client(
        "s3",
        region_name="ap-southeast-2",
        aws_access_key_id="synthetic",
        aws_secret_access_key="synthetic",
    )
    try:
        checksum = sha256(payload).digest()
        key = "relay-integrity/run/tenant=synthetic/date=2026-09-18/receipt=one.dcm"
        with Stubber(storage.client) as stub:
            stub.add_response(
                "put_object",
                {},
                {
                    "Bucket": "synthetic-test-bucket",
                    "Key": key,
                    "Body": payload,
                    "ContentLength": len(payload),
                    "ContentType": "application/dicom",
                    "ChecksumSHA256": base64.b64encode(checksum).decode("ascii"),
                    "IfNoneMatch": "*",
                },
            )
            stored = storage.put_s3(
                source,
                context=ReceiptContext(
                    "one", "synthetic", "RELAY_HTTPS", "2026-09-18T00:00:00Z"
                ),
                received_at=datetime(2026, 9, 18, tzinfo=timezone.utc),
            )
            assert stored.byte_size == len(payload)
            assert stored.checksum_sha256 == checksum.hex()
            assert stored.landing_location == "s3://synthetic-test-bucket/" + key
            stub.assert_no_pending_responses()
    finally:
        storage.client.close()


def test_invalid_application_checkout_never_falls_back(tmp_path):
    result = subprocess.run(
        [sys.executable, "-c", "from integration_tests import backend"],
        cwd=Path(__file__).resolve().parents[1],
        env={**os.environ, "RELAY_INTEGRITY_INGEST_SOURCE": str(tmp_path)},
        capture_output=True,
        check=False,
        timeout=10,
    )
    assert result.returncode != 0
    assert b"must contain ingest_service/relay_ingest.py" in result.stderr
