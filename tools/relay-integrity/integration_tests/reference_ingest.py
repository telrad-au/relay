"""Test-only HTTPS receiver. This is NOT the production cloud implementation.

It supplies the Relay receipt protocol and real local/S3 storage for public CI
and SDK qualification. Application qualification explicitly selects that source
through backend.py instead. Never run this server on a public interface.
"""

import asyncio
import base64
from dataclasses import dataclass, field
from datetime import datetime
from hashlib import sha256
from io import BytesIO
import os
from pathlib import Path
from urllib.parse import quote

from botocore.config import Config
from botocore.session import Session
from fastapi import APIRouter, HTTPException, Request
import pydicom


@dataclass
class S3LandingConfig:
    region: str = "ap-southeast-2"
    bucket: str = ""
    prefix: str = ""
    kms_key_id: str = ""
    timeout_seconds: float = 15


@dataclass
class StorageConfig:
    data_dir: Path


@dataclass
class ScratchConfig:
    min_free_bytes: int
    max_reserved_bytes: int


@dataclass
class AppConfig:
    landing_storage_backend: str = "local"
    s3_landing: S3LandingConfig = field(default_factory=S3LandingConfig)
    storage: StorageConfig | None = None
    scratch: ScratchConfig | None = None


@dataclass
class ReceiptContext:
    receipt_id: str
    tenant_id: str
    transport: str
    received_at: str


@dataclass
class StoredObject:
    landing_location: str
    byte_size: int
    checksum_sha256: str


class ReceiptWriteError(RuntimeError):
    def __init__(self, message, *, durability_failure=False):
        super().__init__(message)


class LandingStorage:
    def __init__(self, config):
        self.config = config
        self.client = None

    def _s3(self):
        if self.client is None:
            self.client = Session().create_client(
                "s3",
                region_name=self.config.s3_landing.region,
                config=Config(
                    connect_timeout=10, read_timeout=30, retries={"max_attempts": 2}
                ),
            )
        return self.client

    def _s3_key(self, tenant_id, receipt_id, received_at):
        return (
            f"{self.config.s3_landing.prefix}/tenant={quote(tenant_id, safe='')}/"
            f"date={received_at:%Y-%m-%d}/receipt={receipt_id}.dcm"
        )

    def put_s3(self, file_path, *, context, received_at):
        payload = file_path.read_bytes()
        checksum = sha256(payload).hexdigest()
        key = self._s3_key(context.tenant_id, context.receipt_id, received_at)
        options = {}
        if self.config.s3_landing.kms_key_id:
            options = {
                "ServerSideEncryption": "aws:kms",
                "SSEKMSKeyId": self.config.s3_landing.kms_key_id,
            }
        self._s3().put_object(
            Bucket=self.config.s3_landing.bucket,
            Key=key,
            Body=payload,
            ContentLength=len(payload),
            ContentType="application/dicom",
            ChecksumSHA256=base64.b64encode(bytes.fromhex(checksum)).decode("ascii"),
            IfNoneMatch="*",
            **options,
        )
        return StoredObject(
            f"s3://{self.config.s3_landing.bucket}/{key}", len(payload), checksum
        )


class ReceiptWriter:
    def __init__(self, config, api, storage):
        self.config, self.storage = config, storage

    def receive_file(
        self, *, source_path, receipt_id, tenant_id, received_at, **kwargs
    ):
        if self.config.landing_storage_backend == "s3":
            return self.storage.put_s3(
                source_path,
                context=ReceiptContext(
                    receipt_id, tenant_id, "RELAY_HTTPS", received_at.isoformat()
                ),
                received_at=received_at,
            )
        destination = self.config.storage.data_dir / (receipt_id + ".dcm")
        destination.parent.mkdir(parents=True, exist_ok=True)
        with destination.open("xb") as target:
            target.write(source_path.read_bytes())
            target.flush()
            os.fsync(target.fileno())
        fd = os.open(destination.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
        readback = destination.read_bytes()
        return StoredObject(
            str(destination), len(readback), sha256(readback).hexdigest()
        )


class RelayIngestRuntime:
    def __init__(self, drain_seconds):
        pass


def build_relay_ingest_router(config, runtime, *, api_client, writer):
    router = APIRouter()

    @router.post("/v1/relay/ingest/dicom", status_code=201)
    async def ingest(request: Request):
        if request.headers.get("content-type") != "application/dicom":
            raise HTTPException(415)
        if request.headers.get("x-telrad-protocol-version") != "1":
            raise HTTPException(400)
        try:
            identity = api_client.authenticate_relay(
                authorization=request.headers.get("authorization"), protocol="dicom"
            )
        except RuntimeError:
            raise HTTPException(401) from None
        # Bounded synthetic receiver, not a replacement for production streaming.
        payload = bytearray()
        async for chunk in request.stream():
            payload.extend(chunk)
            if len(payload) > 16 * 1024**2:
                raise HTTPException(413)
        try:
            dataset = pydicom.dcmread(BytesIO(payload))
            if (
                dataset.file_meta.MediaStorageSOPInstanceUID != dataset.SOPInstanceUID
                or dataset.file_meta.MediaStorageSOPClassUID != dataset.SOPClassUID
            ):
                raise ValueError("file meta mismatch")
        except Exception:
            raise HTTPException(422) from None
        checksum = sha256(payload).hexdigest()
        arrival = api_client.create_relay_dicom_arrival(
            payload_sha256=checksum, payload_size_bytes=len(payload)
        )
        receipt_id = arrival["receiptId"]
        source = config.storage.data_dir / (receipt_id + ".upload")
        source.parent.mkdir(parents=True, exist_ok=True)
        source.write_bytes(payload)
        try:
            stored = await asyncio.to_thread(
                writer.receive_file,
                source_path=source,
                receipt_id=receipt_id,
                tenant_id=identity["companyId"],
                received_at=datetime.fromisoformat(arrival["receiptCreatedAt"]),
            )
            if stored.byte_size != len(payload) or stored.checksum_sha256 != checksum:
                raise ReceiptWriteError("storage checksum mismatch")
            api_client.record_receipt(
                {"id": receipt_id, "filePath": stored.landing_location}
            )
            await asyncio.to_thread(
                api_client.complete_relay_dicom_receipt,
                receipt_id=receipt_id,
                landing_receipt_id=receipt_id,
            )
        except Exception:
            raise HTTPException(503) from None
        finally:
            source.unlink(missing_ok=True)
        return {"status": "accepted", "receiptId": receipt_id}

    return router
