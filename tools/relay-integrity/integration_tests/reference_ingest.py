"""Loopback-only test receiver: validate uploads in memory, then issue receipts."""

import asyncio
from hashlib import sha256

from fastapi import APIRouter, HTTPException, Request

from .image_integrity import dataset_bytes, encode, verify


def build_relay_ingest_router(api, credential):
    router = APIRouter()

    @router.post("/v1/relay/ingest/dicom", status_code=201)
    async def ingest(request: Request):
        if request.headers.get("content-type") != "application/dicom":
            raise HTTPException(415)
        if request.headers.get("x-telrad-protocol-version") != "1":
            raise HTTPException(400)
        if request.headers.get("authorization") != f"Bearer {credential}":
            raise HTTPException(401)
        payload = bytearray()
        async for chunk in request.stream():
            payload.extend(chunk)
            if len(payload) > 16 * 1024**2:
                raise HTTPException(413)
        payload = bytes(payload)
        checksum = sha256(payload).hexdigest()
        arrival = api.create_relay_dicom_arrival(
            payload_sha256=checksum, payload_size_bytes=len(payload)
        )
        receipt = arrival["receiptId"]
        api.payloads[receipt] = payload
        if api.reject_upload:
            raise HTTPException(503)
        try:
            case = api.expected_case
            verify(case, dataset_bytes(encode(case.dataset)), payload, checksum)
        except Exception:
            raise HTTPException(422) from None
        api.record_receipt({"id": receipt})
        await asyncio.to_thread(
            api.complete_relay_dicom_receipt,
            receipt_id=receipt,
            landing_receipt_id=receipt,
        )
        return {"status": "accepted", "receiptId": receipt}

    return router
