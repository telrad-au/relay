"""Exercise the public test receiver's real SDK request construction offline."""

import base64
from datetime import datetime, timezone
from hashlib import sha256
import os
from pathlib import Path
import subprocess
import sys

from botocore.session import Session
from botocore.stub import Stubber

from .reference_ingest import AppConfig, LandingStorage, ReceiptContext, S3LandingConfig


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
        timeout=10,
    )
    assert result.returncode != 0
    assert b"must contain ingest_service/relay_ingest.py" in result.stderr
