"""The receiver compares uploads before acknowledging them, without storage."""

import asyncio
from copy import deepcopy

from fastapi import HTTPException
from starlette.requests import Request
import pytest

from .image_integrity import encode, fixtures
from .reference_ingest import build_relay_ingest_router
from .relay_image_integrity import CREDENTIAL, ReceiptAPI


@pytest.fixture(scope="module")
def image():
    return fixtures()[0]


def upload(api, payload):
    async def receive():
        return {"type": "http.request", "body": payload, "more_body": False}

    request = Request(
        {
            "type": "http",
            "headers": [
                (b"content-type", b"application/dicom"),
                (b"x-telrad-protocol-version", b"1"),
                (b"authorization", f"Bearer {CREDENTIAL}".encode()),
            ],
        },
        receive,
    )
    router = build_relay_ingest_router(api, CREDENTIAL)
    return asyncio.run(router.routes[0].endpoint(request))


def test_receiver_validates_and_captures_exact_upload_in_memory(image):
    api = ReceiptAPI()
    api.expected_case = image
    payload = encode(image.dataset)
    response = upload(api, payload)
    receipt = response["receiptId"]
    assert response["status"] == "accepted"
    assert api.payloads[receipt] == payload
    assert api.completed == {receipt}


@pytest.mark.parametrize("change", ["pixels", "metadata", "header", "truncated"])
def test_changed_upload_never_gets_a_receipt(image, change):
    api = ReceiptAPI()
    api.expected_case = image
    altered = deepcopy(image.dataset)
    if change == "pixels":
        altered.PixelData = bytes([altered.PixelData[0] ^ 1]) + altered.PixelData[1:]
    elif change == "metadata":
        altered.PatientName = "SYNTHETIC^Changed"
    payload = encode(altered)
    if change == "header":
        payload = payload[:128] + b"FAIL" + payload[132:]
    elif change == "truncated":
        payload = payload[:-1]
    with pytest.raises(HTTPException) as caught:
        upload(api, payload)
    assert caught.value.status_code == 422
    assert not api.completed
    assert not api.receipts
    assert list(api.payloads.values()) == [payload]
