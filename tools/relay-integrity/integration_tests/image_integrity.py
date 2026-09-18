"""Synthetic fixtures and comparisons independent of the ingest implementation."""

from dataclasses import dataclass
from hashlib import sha256
from io import BytesIO
import struct

import numpy as np
import pydicom
from pydicom.dataset import Dataset, FileMetaDataset
from pydicom.uid import (
    ExplicitVRLittleEndian,
    ImplicitVRLittleEndian,
    MultiFrameGrayscaleWordSecondaryCaptureImageStorage,
    MultiFrameTrueColorSecondaryCaptureImageStorage,
    RLELossless,
    generate_uid,
)


class IntegrityFailure(RuntimeError):
    """Messages contain only test case labels and comparison names."""


def require(condition, message):
    if not condition:
        raise IntegrityFailure(message)


def digest(data):
    return sha256(data).hexdigest()


def dataset_bytes(part10: bytes) -> bytes:
    """Skip only group 0002, without parsing/re-encoding the actual dataset."""
    require(len(part10) >= 144 and part10[128:132] == b"DICM", "missing Part 10 header")
    offset = 132
    long_vrs = {
        b"OB",
        b"OD",
        b"OF",
        b"OL",
        b"OV",
        b"OW",
        b"SQ",
        b"UC",
        b"UN",
        b"UR",
        b"UT",
    }
    while offset + 8 <= len(part10):
        group, _ = struct.unpack_from("<HH", part10, offset)
        if group != 2:
            return part10[offset:]
        vr = part10[offset + 4 : offset + 6]
        if vr in long_vrs:
            require(offset + 12 <= len(part10), "truncated file meta")
            length = struct.unpack_from("<I", part10, offset + 8)[0]
            offset += 12
        else:
            length = struct.unpack_from("<H", part10, offset + 6)[0]
            offset += 8
        require(
            length % 2 == 0 and offset + length <= len(part10),
            "invalid file meta length",
        )
        offset += length
    raise IntegrityFailure("missing dataset")


def encode(dataset):
    output = BytesIO()
    pydicom.dcmwrite(output, dataset, enforce_file_format=True)
    return output.getvalue()


@dataclass
class ImageCase:
    name: str
    dataset: Dataset
    pixels: np.ndarray


def fixtures():
    """No downloaded images or clinic data; distinct values in every frame."""
    study = generate_uid()
    result = []
    for index, (name, syntax, colour, frames, size) in enumerate(
        [
            ("explicit-16bit", ExplicitVRLittleEndian, False, 1, 64),
            ("implicit-16bit", ImplicitVRLittleEndian, False, 1, 64),
            ("multiframe-16bit", ExplicitVRLittleEndian, False, 5, 64),
            ("colour-multiframe", ExplicitVRLittleEndian, True, 3, 64),
            ("rle-multiframe", RLELossless, False, 4, 64),
            ("fragmented-large", ExplicitVRLittleEndian, False, 4, 1024),
        ]
    ):
        ds = Dataset()
        ds.SpecificCharacterSet = "ISO_IR 192"
        ds.SOPClassUID = (
            MultiFrameTrueColorSecondaryCaptureImageStorage
            if colour
            else MultiFrameGrayscaleWordSecondaryCaptureImageStorage
        )
        ds.SOPInstanceUID = generate_uid()
        ds.StudyInstanceUID = study
        ds.SeriesInstanceUID = generate_uid()
        ds.PatientName = "SYNTHETIC^Café"
        ds.PatientID = "INTEGRITY-ONLY"
        ds.PatientBirthDate = ""
        ds.PatientSex = "O"
        ds.AccessionNumber = "INTEGRITY"
        ds.StudyDate = "20260918"
        ds.StudyTime = "120000"
        ds.StudyID = "INTEGRITY"
        ds.ReferringPhysicianName = ""
        ds.Modality = "OT"
        ds.ConversionType = "WSD"
        ds.Manufacturer = "Synthetic integrity fixture"
        ds.SeriesNumber = index + 1
        ds.InstanceNumber = 1
        ds.ImageType = ["DERIVED", "SECONDARY"]
        ds.BurnedInAnnotation = "NO"
        ds.LossyImageCompression = "00"
        ds.ContentDate = ds.StudyDate
        ds.ContentTime = ds.StudyTime
        ds.Rows = ds.Columns = size
        ds.SamplesPerPixel = 3 if colour else 1
        ds.PhotometricInterpretation = "RGB" if colour else "MONOCHROME2"
        ds.BitsAllocated = ds.BitsStored = 8 if colour else 16
        ds.HighBit = ds.BitsStored - 1
        ds.PixelRepresentation = 0
        ds.NumberOfFrames = frames
        ds.FrameTime = "40"
        ds.FrameIncrementPointer = [0x00181063]
        if colour:
            ds.PlanarConfiguration = 0
        else:
            ds.PresentationLUTShape = "IDENTITY"
            ds.RescaleIntercept = "0"
            ds.RescaleSlope = "1"
            ds.RescaleType = "US"
        shape = (frames, size, size, 3) if colour else (frames, size, size)
        pixels = (
            (
                (np.arange(np.prod(shape), dtype=np.uint32) * 37 + 19)
                % (256 if colour else 65536)
            )
            .astype("u1" if colour else "<u2")
            .reshape(shape)
        )
        for frame in range(frames):
            pixels[frame] ^= frame * 53
        ds.PixelData = pixels.tobytes()
        ds[0x7FE00010].VR = "OB" if colour else "OW"
        ds.add_new(0x00111010, "OB", b"synthetic-private-value\x00")
        ds.add_new(0x00110010, "LO", "TELRAD_INTEGRITY")
        item = Dataset()
        item.CodeValue = "SYNTHETIC"
        item.CodingSchemeDesignator = "99TEST"
        item.CodeMeaning = "Nested metadata café"
        ds.ProcedureCodeSequence = [item]
        ds.file_meta = FileMetaDataset()
        ds.file_meta.TransferSyntaxUID = ExplicitVRLittleEndian
        ds.file_meta.MediaStorageSOPClassUID = ds.SOPClassUID
        ds.file_meta.MediaStorageSOPInstanceUID = ds.SOPInstanceUID
        if syntax == RLELossless:
            ds.compress(RLELossless, generate_instance_uid=False)
        else:
            ds.file_meta.TransferSyntaxUID = syntax
        result.append(ImageCase(name, ds, pixels))
    return result


def verify(case: ImageCase, wire: bytes, landed: bytes, expected_file_hash: str):
    source = dataset_bytes(encode(case.dataset))
    require(source == wire, f"{case.name}: sender changed dataset")
    require(
        digest(landed) == expected_file_hash,
        f"{case.name}: received object SHA-256 mismatch",
    )
    received = pydicom.dcmread(BytesIO(landed))
    meta = received.file_meta
    for key, expected in (
        ("MediaStorageSOPClassUID", case.dataset.SOPClassUID),
        ("MediaStorageSOPInstanceUID", case.dataset.SOPInstanceUID),
        ("TransferSyntaxUID", case.dataset.file_meta.TransferSyntaxUID),
    ):
        require(
            getattr(meta, key, None) == expected,
            f"{case.name}: file meta {key} mismatch",
        )
    require(dataset_bytes(landed) == wire, f"{case.name}: dataset bytes changed")
    # Decode outside Relay and ingest. Compare all frames to the generated source
    # array, not just to another decoding of the potentially faulty fixture.
    decoded = received.pixel_array
    if int(received.NumberOfFrames) == 1:
        decoded = decoded[np.newaxis, ...]
    require(
        decoded.shape == case.pixels.shape and np.array_equal(decoded, case.pixels),
        f"{case.name}: decoded pixels changed",
    )
    return {
        "case": case.name,
        "status": "passed",
        "transferSyntax": str(meta.TransferSyntaxUID),
        "bytes": len(landed),
        "datasetSha256": digest(wire),
        "objectSha256": digest(landed),
        "frameSha256": [digest(frame.tobytes()) for frame in decoded],
        "frames": len(decoded),
    }
