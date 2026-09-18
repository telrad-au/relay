"""Offline AWS SDK contracts; these are never labelled live AWS qualification."""

from io import BytesIO
import base64
from pathlib import Path
from types import SimpleNamespace

from botocore.response import StreamingBody
from botocore.session import Session
from botocore.stub import Stubber
import pytest

from .backend import AppConfig, S3LandingConfig, LandingStorage, ReceiptContext

from .image_integrity import IntegrityFailure, digest
from .relay_image_integrity import TrackedStorage, aws_reader, cleanup_s3, read_landed


BUCKET = "synthetic-integrity-bucket"
ACCOUNT = "123456789012"
PREFIX = "relay-integrity/test-run"
KEY = PREFIX + "/tenant=synthetic/date=2026-09-18/receipt=1.dcm"


@pytest.fixture
def client():
    client = Session().create_client(
        "s3",
        region_name="ap-southeast-2",
        aws_access_key_id="synthetic",
        aws_secret_access_key="synthetic",
    )
    yield client
    client.close()


@pytest.fixture
def args():
    return SimpleNamespace(
        storage="aws", s3_bucket=BUCKET, expected_aws_account=ACCOUNT
    )


@pytest.fixture
def storage():
    config = AppConfig(
        landing_storage_backend="s3",
        s3_landing=S3LandingConfig(bucket=BUCKET, prefix=PREFIX),
    )
    storage = TrackedStorage(config)
    storage.attempted_keys.add(KEY)
    return storage


def request():
    return {"Bucket": BUCKET, "Key": KEY, "ExpectedBucketOwner": ACCOUNT}


def test_readback_uses_independent_sdk_get_with_owner_and_checksum(
    client, args, storage
):
    body = StreamingBody(BytesIO(b"synthetic-bytes"), len(b"synthetic-bytes"))
    with Stubber(client) as stub:
        stub.add_response(
            "get_object",
            {
                "Body": body,
                "ContentLength": len(b"synthetic-bytes"),
                "ChecksumSHA256": base64.b64encode(
                    bytes.fromhex(digest(b"synthetic-bytes"))
                ).decode("ascii"),
            },
            {**request(), "ChecksumMode": "ENABLED"},
        )
        assert (
            read_landed(f"s3://{BUCKET}/{KEY}", args, client, storage)
            == b"synthetic-bytes"
        )
        stub.assert_no_pending_responses()
    assert body._raw_stream.closed


@pytest.mark.parametrize(
    "checksum", [None, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="]
)
def test_s3_readback_requires_matching_service_checksum(
    checksum, client, args, storage
):
    body = StreamingBody(BytesIO(b"synthetic"), 9)
    response = {"Body": body, "ContentLength": 9}
    if checksum:
        response["ChecksumSHA256"] = checksum
    with Stubber(client) as stub:
        stub.add_response(
            "get_object", response, {**request(), "ChecksumMode": "ENABLED"}
        )
        with pytest.raises(IntegrityFailure, match="checksum missing or mismatched"):
            read_landed(f"s3://{BUCKET}/{KEY}", args, client, storage)
        stub.assert_no_pending_responses()


@pytest.mark.parametrize(
    "location",
    [
        f"s3://other-bucket/{KEY}",
        f"s3://{BUCKET}/unrelated.dcm",
        "file:///tmp/other.dcm",
    ],
)
def test_readback_rejects_untracked_or_foreign_object(location, client, args, storage):
    with Stubber(client):
        with pytest.raises(IntegrityFailure, match="unexpected landing object"):
            read_landed(location, args, client, storage)


@pytest.mark.parametrize("version", [None, "test-version", "null"])
def test_cleanup_deletes_only_exact_attempted_object_version(
    version, client, args, storage
):
    with Stubber(client) as stub:
        stub.add_response(
            "head_object", {} if version is None else {"VersionId": version}, request()
        )
        stub.add_response(
            "delete_object",
            {},
            request() if version is None else {**request(), "VersionId": version},
        )
        cleanup_s3(args, client, storage)
        stub.assert_no_pending_responses()


def test_cleanup_ignores_missing_failed_put(client, args, storage):
    with Stubber(client) as stub:
        stub.add_client_error(
            "head_object",
            service_error_code="404",
            http_status_code=404,
            expected_params=request(),
        )
        cleanup_s3(args, client, storage)
        stub.assert_no_pending_responses()


def test_cleanup_failure_is_not_reported_as_success(client, args, storage):
    with Stubber(client) as stub:
        stub.add_response("head_object", {}, request())
        stub.add_client_error(
            "delete_object",
            service_error_code="AccessDenied",
            http_status_code=403,
            expected_params=request(),
        )
        with pytest.raises(IntegrityFailure, match="cleanup failed"):
            cleanup_s3(args, client, storage)
        stub.assert_no_pending_responses()


def test_cleanup_cannot_delete_outside_run_prefix(client, args, storage):
    storage.attempted_keys = {"unrelated/receipt=1.dcm"}
    with Stubber(client):
        with pytest.raises(IntegrityFailure, match="cleanup failed"):
            cleanup_s3(args, client, storage)


def test_ambiguous_put_is_tracked_before_sdk_call(monkeypatch, storage):
    from datetime import datetime, timezone

    def fail(*_args, **_kwargs):
        raise TimeoutError("synthetic ambiguous PUT")

    monkeypatch.setattr(LandingStorage, "put_s3", fail)
    storage.attempted_keys.clear()
    context = ReceiptContext(
        receipt_id="1",
        tenant_id="synthetic",
        transport="RELAY_HTTPS",
        received_at="2026-09-18T00:00:00Z",
    )
    with pytest.raises(TimeoutError):
        storage.put_s3(
            Path("unused"),
            context=context,
            received_at=datetime(2026, 9, 18, tzinfo=timezone.utc),
        )
    assert storage.attempted_keys == {KEY}


@pytest.mark.parametrize(
    "bucket, account", [(None, ACCOUNT), (BUCKET, None), (BUCKET, "invalid")]
)
def test_aws_mode_requires_explicit_destination_before_credentials(
    bucket, account, storage
):
    with pytest.raises(IntegrityFailure, match="dedicated test bucket"):
        aws_reader(
            SimpleNamespace(s3_bucket=bucket, expected_aws_account=account), storage
        )
