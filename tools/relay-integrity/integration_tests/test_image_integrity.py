"""Negative controls prove that byte/pixel/command comparisons can fail."""

from copy import deepcopy

import pytest
from pydicom.uid import ImplicitVRLittleEndian, generate_uid

from .image_integrity import (
    IntegrityFailure,
    dataset_bytes,
    encode,
    fixtures,
    verify,
    verify_streams,
)


@pytest.fixture(scope="module")
def images():
    return fixtures()


def command(case):
    return {
        "sop_class_uid": str(case.dataset.SOPClassUID),
        "sop_instance_uid": str(case.dataset.SOPInstanceUID),
    }


def compare(case, sent, received, syntax=None, fields=None):
    return verify(
        case,
        sent,
        received,
        syntax or str(case.dataset.file_meta.TransferSyntaxUID),
        fields or command(case),
    )


def test_every_fixture_decodes_to_generated_pixels(images):
    for case in images:
        wire = dataset_bytes(encode(case.dataset))
        result = compare(case, wire, wire)
        assert result["frames"] == len(case.pixels)
        assert len(set(result["frameSha256"])) == len(case.pixels)


@pytest.mark.parametrize(
    "change", ["private-tag", "unicode-tag", "sequence", "pixels", "missing-frame"]
)
def test_changed_dataset_is_rejected(images, change):
    case = images[2]
    source = dataset_bytes(encode(case.dataset))
    altered = deepcopy(case.dataset)
    if change == "private-tag":
        altered[0x00111010].value = b"changed!"
    elif change == "unicode-tag":
        altered.PatientName = "SYNTHETIC^Different"
    elif change == "sequence":
        altered.ProcedureCodeSequence[0].CodeMeaning = "Changed nested value"
    elif change == "pixels":
        altered.PixelData = bytes([altered.PixelData[0] ^ 1]) + altered.PixelData[1:]
    else:
        altered.NumberOfFrames -= 1
        altered.PixelData = altered.PixelData[: -case.pixels[0].nbytes]
    with pytest.raises(IntegrityFailure, match="dataset bytes changed"):
        compare(case, source, dataset_bytes(encode(altered)))


@pytest.mark.parametrize("key", ["sop_class_uid", "sop_instance_uid"])
def test_changed_command_identifier_is_rejected(images, key):
    case = images[0]
    wire = dataset_bytes(encode(case.dataset))
    fields = command(case)
    fields[key] = generate_uid()
    with pytest.raises(IntegrityFailure, match="command .* changed"):
        compare(case, wire, wire, fields=fields)


def test_wrong_negotiated_transfer_syntax_is_rejected(images):
    case = images[0]
    wire = dataset_bytes(encode(case.dataset))
    with pytest.raises(IntegrityFailure, match="transfer syntax changed"):
        compare(case, wire, wire, syntax=str(ImplicitVRLittleEndian))


def test_sender_mutation_and_truncation_are_rejected(images):
    case = images[0]
    wire = dataset_bytes(encode(case.dataset))
    with pytest.raises(IntegrityFailure, match="sender changed"):
        compare(case, wire + b"x", wire)
    with pytest.raises(IntegrityFailure, match="dataset bytes changed"):
        compare(case, wire, wire[:-1])


def test_pixel_comparison_covers_last_frame(images):
    case = deepcopy(images[2])
    wire = dataset_bytes(encode(case.dataset))
    case.pixels[-1, -1, -1] ^= 1
    with pytest.raises(IntegrityFailure, match="decoded pixels changed"):
        compare(case, wire, wire)


@pytest.mark.parametrize("direction", ["sent", "received"])
def test_full_stream_corruption_is_rejected(direction):
    client = {
        "sent": b"association+command+dataset+release",
        "received": b"reply+status+release",
    }
    server = {"sent": client["received"], "received": client["sent"]}
    verify_streams(client, server)
    server[direction] += b"x"
    with pytest.raises(IntegrityFailure, match="stream bytes changed"):
        verify_streams(client, server)


def test_empty_stream_is_not_evidence():
    with pytest.raises(IntegrityFailure, match="missing sender stream"):
        verify_streams({"sent": b"", "received": b""}, {"sent": b"", "received": b""})


@pytest.mark.parametrize("payload", [b"", b"x" * 150, b"\0" * 128 + b"DICM"])
def test_truncated_or_missing_fixture_file_meta_rejected(payload):
    with pytest.raises(IntegrityFailure):
        dataset_bytes(payload)
