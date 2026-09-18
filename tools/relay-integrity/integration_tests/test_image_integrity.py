from copy import deepcopy
from io import BytesIO

import pydicom
import pytest
from pydicom.uid import ImplicitVRLittleEndian, generate_uid

from .image_integrity import (
    IntegrityFailure,
    dataset_bytes,
    digest,
    encode,
    fixtures,
    verify,
)


@pytest.fixture(scope="module")
def images():
    return fixtures()


def test_every_fixture_decodes_to_generated_pixels(images):
    for case in images:
        payload = encode(case.dataset)
        result = verify(case, dataset_bytes(payload), payload, digest(payload))
        assert result["frames"] == len(case.pixels)
        assert len(result["frameSha256"]) == len(case.pixels)
        assert len(set(result["frameSha256"])) == len(case.pixels)


def test_file_wrapper_can_change_without_changing_dataset(images):
    case = images[0]
    source = encode(case.dataset)
    ds = pydicom.dcmread(BytesIO(source))
    ds.file_meta.ImplementationClassUID = generate_uid()
    ds.file_meta.ImplementationVersionName = "TEST_WRITER"
    landed = encode(ds)
    assert source != landed
    verify(case, dataset_bytes(source), landed, digest(landed))


@pytest.mark.parametrize(
    "change", ["private-tag", "unicode-tag", "sequence", "pixels", "missing-frame"]
)
def test_changed_dataset_is_rejected_even_with_matching_storage_hash(images, change):
    case = images[2]
    source = encode(case.dataset)
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
    landed = encode(altered)
    with pytest.raises(IntegrityFailure, match="dataset bytes changed"):
        verify(case, dataset_bytes(source), landed, digest(landed))


@pytest.mark.parametrize(
    "tag",
    ["TransferSyntaxUID", "MediaStorageSOPInstanceUID", "MediaStorageSOPClassUID"],
)
def test_incorrect_file_meta_rejected(images, tag):
    case = images[0]
    source = encode(case.dataset)
    # Rewrite header only; keep the source dataset byte-for-byte identical.
    altered = deepcopy(case.dataset)
    if tag == "TransferSyntaxUID":
        altered.file_meta.TransferSyntaxUID = ImplicitVRLittleEndian
    else:
        setattr(
            altered,
            "SOPInstanceUID" if tag.endswith("InstanceUID") else "SOPClassUID",
            generate_uid(),
        )
    rewritten = encode(altered)
    landed = rewritten[: -len(dataset_bytes(rewritten))] + dataset_bytes(source)
    with pytest.raises(IntegrityFailure, match="file meta .* mismatch"):
        verify(case, dataset_bytes(source), landed, digest(landed))


def test_sender_mutation_and_storage_corruption_are_rejected(images):
    case = images[0]
    source = encode(case.dataset)
    with pytest.raises(IntegrityFailure, match="sender changed"):
        verify(case, dataset_bytes(source) + b"x", source, digest(source))
    with pytest.raises(IntegrityFailure, match="SHA-256 mismatch"):
        verify(
            case,
            dataset_bytes(source),
            source[:-1] + bytes([source[-1] ^ 1]),
            digest(source),
        )


def test_pixel_comparison_covers_last_frame(images):
    case = deepcopy(images[2])
    source = encode(case.dataset)
    case.pixels[-1, -1, -1] ^= 1
    with pytest.raises(IntegrityFailure, match="decoded pixels changed"):
        verify(case, dataset_bytes(source), source, digest(source))


@pytest.mark.parametrize("payload", [b"", b"x" * 150, b"\0" * 128 + b"DICM"])
def test_truncated_or_missing_file_meta_rejected(payload):
    with pytest.raises(IntegrityFailure):
        dataset_bytes(payload)
