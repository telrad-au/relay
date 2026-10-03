"""Real, synthetic DICOM exchanges qualify the mutual-TLS test receiver."""

import socket
import ssl
import subprocess
import threading
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
from hashlib import sha256

import pytest
from pydicom.dataset import Dataset, FileMetaDataset
from pydicom.uid import (
    ExplicitVRLittleEndian,
    ImplicitVRLittleEndian,
    SecondaryCaptureImageStorage,
    generate_uid,
)
from pynetdicom import AE, evt
from pynetdicom.dsutils import encode

from .tls_receiver import tls_receiver


@pytest.fixture
def dataset():
    dataset = Dataset()
    dataset.SOPClassUID = SecondaryCaptureImageStorage
    dataset.SOPInstanceUID = generate_uid()
    dataset.PatientName = "SYNTHETIC^Integrity"
    dataset.PatientID = "INTEGRITY-TEST-ONLY"
    dataset.Rows = 32
    dataset.Columns = 32
    dataset.SamplesPerPixel = 1
    dataset.PhotometricInterpretation = "MONOCHROME2"
    dataset.BitsAllocated = dataset.BitsStored = 8
    dataset.HighBit = 7
    dataset.PixelRepresentation = 0
    dataset.PixelData = bytes(range(256)) * 4
    dataset[0x7FE00010].VR = "OB"
    dataset.file_meta = FileMetaDataset()
    dataset.file_meta.TransferSyntaxUID = ExplicitVRLittleEndian
    return dataset


def send(receiver, dataset, *, with_certificate=True, response_received=None):
    sent, received, datasets = bytearray(), bytearray(), []

    def capture_dataset(event):
        stream = event.message.data_set
        if stream is not None:
            datasets.append(stream.getvalue())

    ae = AE(ae_title="INTEGRITY_SCU")
    ae.acse_timeout = ae.dimse_timeout = ae.network_timeout = 5
    ae.add_requested_context(dataset.SOPClassUID, dataset.file_meta.TransferSyntaxUID)
    association = ae.associate(
        "127.0.0.1",
        receiver.port,
        ae_title="INTEGRITY_SCP",
        max_pdu=512,
        tls_args=(
            receiver.client_context(with_certificate=with_certificate),
            "127.0.0.1",
        ),
        evt_handlers=[
            (evt.EVT_DATA_SENT, lambda event: sent.extend(event.data)),
            (evt.EVT_DATA_RECV, lambda event: received.extend(event.data)),
            (evt.EVT_DIMSE_SENT, capture_dataset),
        ],
    )
    if not association.is_established:
        ae.shutdown()
        return None, bytes(sent), bytes(received), datasets
    try:
        status = association.send_c_store(dataset, msg_id=41, priority=1)
        if response_received is not None:
            response_received.set()
        association.release()
        return status.Status, bytes(sent), bytes(received), datasets
    finally:
        ae.shutdown()


@pytest.mark.parametrize("syntax", [ExplicitVRLittleEndian, ImplicitVRLittleEndian])
def test_raw_dataset_and_both_complete_streams_are_captured(tmp_path, dataset, syntax):
    dataset.file_meta.TransferSyntaxUID = syntax
    with tls_receiver(tmp_path) as receiver:
        status, sent, received, datasets = send(receiver, dataset)
        assert status == 0x0000
        assert len(receiver.arrivals) == 1
        arrival = receiver.arrivals[0]
        assert (
            arrival["dataset"]
            == datasets[0]
            == encode(dataset, syntax.is_implicit_VR, True)
        )
        assert arrival["transfer_syntax"] == str(syntax)
        assert arrival["command"] == {
            "sop_class_uid": str(dataset.SOPClassUID),
            "sop_instance_uid": str(dataset.SOPInstanceUID),
            "message_id": 41,
            "priority": 1,
        }
        expected_fingerprint = sha256(
            ssl.PEM_cert_to_DER_cert(receiver.client_cert_pem)
        ).hexdigest()
        assert arrival["peer_fingerprint"] == expected_fingerprint
        assert receiver.wait_for_closed(arrival)
        stream = receiver.streams_for(arrival)
        assert stream["received"] == sent
        assert stream["sent"] == received
        assert stream["peer_fingerprint"] == expected_fingerprint
        assert stream["closed"]
        assert sent[0] == 0x01  # A-ASSOCIATE-RQ
        assert received[0] == 0x02  # A-ASSOCIATE-AC
        assert sent[-10] == 0x05  # A-RELEASE-RQ
        assert received[-10] == 0x06  # A-RELEASE-RP
        # Returned snapshots cannot corrupt internal capture state.
        receiver.arrivals[0]["command"]["message_id"] = 0
        receiver.streams[arrival["association_id"]]["received"] = b""
        assert receiver.arrivals[0]["command"]["message_id"] == 41
        assert receiver.streams_for(arrival)["received"] == sent


@pytest.mark.parametrize("status", [0xA700, 0xC123, 0xB000])
def test_receiver_returns_exact_selected_status(tmp_path, dataset, status):
    with tls_receiver(tmp_path) as receiver:
        receiver.status = status
        actual, *_ = send(receiver, dataset)
        assert actual == status
        assert len(receiver.arrivals) == 1


def test_response_can_be_held_until_caller_releases_it(tmp_path, dataset):
    with tls_receiver(tmp_path) as receiver:
        receiver.hold_response = True
        response_received = threading.Event()
        with ThreadPoolExecutor(max_workers=1) as executor:
            pending = executor.submit(
                send, receiver, dataset, response_received=response_received
            )
            try:
                assert receiver.response_entered.wait(5)
                assert len(receiver.arrivals) == 1
                assert not response_received.wait(0.2)
            finally:
                receiver.release_response.set()
            assert pending.result(timeout=5)[0] == 0x0000
            assert response_received.is_set()


def assert_tls_rejected(receiver, context):
    # TLS 1.2 reports missing/untrusted client certificates during the handshake;
    # TLS 1.3 may defer the alert until the client's first read. Use the standard
    # library here so rejected handshakes always close, including when shutdown()
    # raises (pynetdicom 3.0.4's failed-client cleanup can skip close in that case).
    context.maximum_version = ssl.TLSVersion.TLSv1_2
    with (
        socket.create_connection(("127.0.0.1", receiver.port), timeout=5) as raw,
        pytest.raises(ssl.SSLError),
        context.wrap_socket(raw, server_hostname="127.0.0.1"),
    ):
        pytest.fail("receiver accepted an unauthorized TLS client")


def test_missing_client_certificate_never_establishes_an_association(tmp_path, dataset):
    with tls_receiver(tmp_path) as receiver:
        assert_tls_rejected(receiver, receiver.client_context(with_certificate=False))
        assert receiver.arrivals == []
        assert receiver.streams == {}
        # A failed TLS handshake must not stop the listener.
        assert send(receiver, dataset)[0] == 0x0000


def test_client_certificate_from_another_ca_is_rejected(tmp_path, dataset):
    with tls_receiver(tmp_path) as receiver, tls_receiver(tmp_path) as unrelated:
        context = receiver.client_context(with_certificate=False)
        context.load_cert_chain(
            unrelated._paths["client_cert"], unrelated._paths["client_key"]
        )
        assert_tls_rejected(receiver, context)
        assert receiver.arrivals == []
        assert receiver.streams == {}
        assert receiver.ca_pem != unrelated.ca_pem
        assert send(receiver, dataset)[0] == 0x0000


def test_certificates_have_required_constraints_and_are_removed(tmp_path):
    with tls_receiver(tmp_path) as receiver:
        temporary = receiver._paths["ca_cert"].parent
        assert receiver.client_key_pem.startswith("-----BEGIN " + "EC PRIVATE KEY-----")
        expiry = datetime.fromisoformat(receiver.not_after.replace("Z", "+00:00"))
        assert expiry > datetime.now(timezone.utc) + timedelta(days=89)
        assert expiry <= datetime.now(timezone.utc) + timedelta(days=90)
        for name, purpose in [("server", "sslserver"), ("client", "sslclient")]:
            result = subprocess.run(
                [
                    "openssl",
                    "verify",
                    "-CAfile",
                    str(receiver._paths["ca_cert"]),
                    "-purpose",
                    purpose,
                    str(receiver._paths[f"{name}_cert"]),
                ],
                check=True,
                capture_output=True,
                text=True,
                timeout=15,
            )
            assert ": OK" in result.stdout
        server = subprocess.run(
            [
                "openssl",
                "x509",
                "-in",
                str(receiver._paths["server_cert"]),
                "-noout",
                "-ext",
                "subjectAltName",
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=15,
        ).stdout
        assert "IP Address:127.0.0.1" in server
        assert "DNS:host.docker.internal" in server
        client = subprocess.run(
            [
                "openssl",
                "pkey",
                "-in",
                str(receiver._paths["client_key"]),
                "-text",
                "-noout",
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=15,
        ).stdout
        assert "prime256v1" in client
    assert not temporary.exists()


def test_association_streams_are_separate_for_repeated_objects(tmp_path, dataset):
    with tls_receiver(tmp_path) as receiver:
        results = [send(receiver, dataset), send(receiver, dataset)]
        arrivals = receiver.arrivals
        assert len(arrivals) == 2
        assert arrivals[0]["association_id"] != arrivals[1]["association_id"]
        assert arrivals[0]["dataset"] == arrivals[1]["dataset"]
        for arrival, (_, sent, received, _) in zip(arrivals, results):
            assert receiver.wait_for_closed(arrival)
            assert receiver.streams_for(arrival)["received"] == sent
            assert receiver.streams_for(arrival)["sent"] == received
