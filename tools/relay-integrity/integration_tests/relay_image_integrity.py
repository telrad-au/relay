"""Verify raw DICOM byte preservation through Relay's mutual-TLS TCP proxy."""

import argparse
import json
import os
import platform
import signal
import socket
import ssl
import subprocess
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack, contextmanager
from copy import deepcopy
from datetime import datetime, timezone
from pathlib import Path
from urllib.error import URLError
from urllib.request import ProxyHandler, build_opener
from uuid import uuid4

from pynetdicom import AE, evt

from .container_target import packaged_process
from .image_integrity import (
    ImageCase,
    IntegrityFailure,
    digest,
    fixtures,
    require,
    verify,
    verify_streams,
)
from .tls_receiver import tls_receiver

LIMITATIONS = [
    "A generated, pre-paired identity is used; enrolment and renewal are not exercised.",
    "The upstream is a synthetic mutual-TLS DICOM SCP, not the Telrad platform or a clinic PACS.",
    "Native foreground binaries and the release Dockerfile are tested, not installed services.",
    "Synthetic uncompressed/RLE fixtures do not qualify every modality or codec.",
    "HL7, report pickup, ledger authorization and application storage are outside this DICOM suite.",
]


def free_ports(count):
    # Reserve all chosen ports together so the fixture never reuses one locally.
    with ExitStack() as stack:
        sockets = [stack.enter_context(socket.socket()) for _ in range(count)]
        for sock in sockets:
            sock.bind(("127.0.0.1", 0))
        return [sock.getsockname()[1] for sock in sockets]


def write_configuration(directory, receiver):
    dicom, hl7, status, unused = free_ports(4)
    identity = {
        "schemaVersion": 1,
        "relayId": "integrity-relay",
        "privateKey": receiver.client_key_pem,
        "certificate": receiver.client_cert_pem,
        "notAfter": receiver.not_after,
        "telrad": {
            "host": "127.0.0.1",
            "dicomPort": receiver.port,
            "hl7Port": unused,
            "reportPort": unused,
            "caCertificate": receiver.ca_pem,
        },
    }
    config = {
        "schemaVersion": 6,
        "dataDir": ".",
        # Fail locally if a regression unexpectedly attempts pairing or renewal.
        "enrolmentUrl": f"https://127.0.0.1:{unused}/enrolments",
        "listenAddress": "127.0.0.1",
        "dicomPort": dicom,
        "hl7Port": hl7,
        "reportHost": "127.0.0.1",
        "reportPort": unused,
        "statusAddress": f"127.0.0.1:{status}",
        "connectTimeoutSeconds": 2,
    }
    for name, value in (("identity.json", identity), ("relay.json", config)):
        path = directory / name
        path.write_text(json.dumps(value), encoding="utf-8")
        path.chmod(0o600)
    return config


@contextmanager
def relay_process(binary, directory, receiver, image=None, report=None):
    config = write_configuration(directory, receiver)
    # Developer/CI configuration must never redirect the synthetic fixture.
    environment = {
        key: value
        for key, value in os.environ.items()
        if not key.upper().startswith("TELRAD_RELAY_")
    }
    with ExitStack() as stack:
        if image:
            process = stack.enter_context(packaged_process(image, directory, report))
        else:
            process = subprocess.Popen(
                [str(binary), "--config", str(directory / "relay.json"), "run"],
                env=environment,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        try:
            deadline = time.monotonic() + 15
            # /readyz also requires report pickup, intentionally outside this suite.
            # Poll status rather than opening an extra DICOM association.
            opener = build_opener(ProxyHandler({}))
            while True:
                require(process.poll() is None, "Relay exited before listening")
                try:
                    with opener.open(
                        f"http://{config['statusAddress']}/status", timeout=0.5
                    ) as response:
                        status = json.load(response)
                    if status.get("paired") and status.get("listeners", {}).get(
                        "dicom"
                    ):
                        break
                except (OSError, URLError):
                    pass
                require(time.monotonic() < deadline, "Relay listener timed out")
                time.sleep(0.05)
            yield config["dicomPort"]
        finally:
            if not image:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


def send(case, port, response_received=None):
    datasets = []
    streams = {"sent": bytearray(), "received": bytearray()}

    def capture(event):
        if event.message.command_set.CommandField == 0x0001:
            datasets.append(event.message.data_set.getvalue())

    def sent(event):
        streams["sent"].extend(event.data)

    def received(event):
        streams["received"].extend(event.data)

    ae = AE(ae_title="INTEGRITY_SCU")
    ae.acse_timeout = 10
    ae.dimse_timeout = 30
    ae.network_timeout = 30
    ae.add_requested_context(
        case.dataset.SOPClassUID, case.dataset.file_meta.TransferSyntaxUID
    )
    association = ae.associate(
        "127.0.0.1",
        port,
        ae_title="INTEGRITY_SCP",
        evt_handlers=[
            (evt.EVT_DIMSE_SENT, capture),
            (evt.EVT_DATA_SENT, sent),
            (evt.EVT_DATA_RECV, received),
        ],
    )
    require(association.is_established, f"{case.name}: association rejected")
    try:
        status = association.send_c_store(case.dataset)
        if response_received is not None:
            response_received.set()
        require(len(datasets) == 1, f"{case.name}: missing sent dataset capture")
        code = getattr(status, "Status", None)
    finally:
        if association.is_established:
            association.release()
        else:
            association.abort()
    require(association.is_released, f"{case.name}: association did not release")
    return code, datasets[0], {key: bytes(value) for key, value in streams.items()}


def record_case(case, result, receiver, before, expected_status, report, name=None):
    code, sent, streams = result
    require(code == expected_status, f"{case.name}: C-STORE status changed")
    arrivals = receiver.arrivals
    require(len(arrivals) == before + 1, f"{case.name}: expected exactly one arrival")
    arrival = arrivals[-1]
    require(receiver.wait_for_closed(arrival), "upstream association did not close")
    expected_peer = digest(ssl.PEM_cert_to_DER_cert(receiver.client_cert_pem))
    require(
        arrival["peer_fingerprint"] == expected_peer, "unexpected TLS client identity"
    )
    evidence = verify(
        case, sent, arrival["dataset"], arrival["transfer_syntax"], arrival["command"]
    )
    evidence.update(verify_streams(streams, receiver.streams_for(arrival)))
    evidence["dicomStatus"] = code
    evidence["mutualTLS"] = True
    evidence["clientCertificateSha256"] = expected_peer
    if name:
        evidence["case"] = name
    report["cases"].append(evidence)


def exercise(port, receiver, report):
    cases = fixtures()
    cases.append(ImageCase("identical-repeat", cases[0].dataset, cases[0].pixels))
    changed = deepcopy(cases[0].dataset)
    pixels = cases[0].pixels.copy()
    pixels.flat[0] ^= 1
    changed.PixelData = pixels.tobytes()
    cases.append(ImageCase("same-uid-different-pixels", changed, pixels))
    for case in cases:
        before = len(receiver.arrivals)
        record_case(case, send(case, port), receiver, before, 0, report)

    # Only the upstream SCP decides C-STORE success. Relay must not acknowledge early.
    receiver.hold_response = True
    receiver.response_entered.clear()
    receiver.release_response.clear()
    before = len(receiver.arrivals)
    with ThreadPoolExecutor(max_workers=1) as executor:
        response_received = threading.Event()
        future = executor.submit(send, cases[0], port, response_received)
        try:
            require(
                receiver.response_entered.wait(10), "upstream response gate not reached"
            )
            time.sleep(0.2)
            require(
                not response_received.is_set() and not future.done(),
                "C-STORE completed before upstream response",
            )
        finally:
            receiver.release_response.set()
        result = future.result(timeout=40)
    receiver.hold_response = False
    record_case(
        cases[0], result, receiver, before, 0, report, "waits-for-upstream-status"
    )

    # Preserve a specific failure, rather than merely returning any nonzero status.
    receiver.status = 0xA700
    before = len(receiver.arrivals)
    record_case(
        cases[0],
        send(cases[0], port),
        receiver,
        before,
        0xA700,
        report,
        "upstream-rejection",
    )
    receiver.status = 0
    require(len(receiver.arrivals) == len(cases) + 2, "arrival inventory mismatch")
    report["attemptedArrivals"] = len(receiver.arrivals)
    report["successfulStores"] = len(cases) + 1
    report["rejectedStores"] = 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    target = parser.add_mutually_exclusive_group(required=True)
    target.add_argument("--relay-binary", type=Path)
    target.add_argument("--relay-image")
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()

    def interrupted(_signal, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    report = {
        "schemaVersion": 3,
        "runId": str(uuid4()),
        "status": "failed",
        "transport": "dicom-over-mutual-tls",
        "target": "docker" if args.relay_image else "native",
        "platform": {"system": platform.system(), "machine": platform.machine()},
        "startedAt": datetime.now(timezone.utc).isoformat(),
        "cases": [],
        "limitations": LIMITATIONS,
    }
    try:
        if args.relay_binary:
            args.relay_binary = args.relay_binary.resolve(strict=True)
            require(
                os.access(args.relay_binary, os.X_OK), "Relay binary is not executable"
            )
            report["relayBinarySha256"] = digest(args.relay_binary.read_bytes())
        root = Path(__file__).resolve().parents[3]
        report["relayRepositoryRevision"] = subprocess.check_output(
            ["git", "-C", str(root), "rev-parse", "HEAD"], text=True, timeout=10
        ).strip()
        report["relayRepositoryDirty"] = bool(
            subprocess.check_output(
                ["git", "-C", str(root), "status", "--porcelain"], text=True, timeout=10
            ).strip()
        )
        with tempfile.TemporaryDirectory(prefix="relay-integrity-") as temporary:
            directory = Path(temporary)
            with (
                tls_receiver(directory) as receiver,
                relay_process(
                    args.relay_binary, directory, receiver, args.relay_image, report
                ) as port,
            ):
                exercise(port, receiver, report)
        report["status"] = "passed"
    except Exception as exc:  # noqa: BLE001 - always persist safe failure evidence
        report["error"] = (
            str(exc) if isinstance(exc, IntegrityFailure) else type(exc).__name__
        )
    except KeyboardInterrupt:
        report["error"] = "interrupted"
    finally:
        report["finishedAt"] = datetime.now(timezone.utc).isoformat()
        args.evidence.parent.mkdir(parents=True, exist_ok=True)
        args.evidence.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"Relay image integrity: {report['status']}; evidence: {args.evidence}")
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
