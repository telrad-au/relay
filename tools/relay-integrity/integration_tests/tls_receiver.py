"""Disposable mutual-TLS DICOM SCP for synthetic, in-memory integrity tests.

The received dataset is captured directly from the DIMSE request, never decoded
and re-encoded. Stream captures concatenate pynetdicom's raw DICOM PDU events,
including association negotiation and release, on the plaintext side of TLS.
They deliberately exclude TLS records and TCP packet boundaries.
"""

import ssl
import subprocess
import tempfile
import threading
from contextlib import contextmanager
from datetime import datetime, timezone
from hashlib import sha256
from pathlib import Path
from uuid import uuid4

from pydicom.uid import ExplicitVRLittleEndian, ImplicitVRLittleEndian, RLELossless
from pynetdicom import AE, StoragePresentationContexts, evt


def _openssl(*arguments):
    return subprocess.run(
        ["openssl", *map(str, arguments)],
        check=True,
        capture_output=True,
        text=True,
        timeout=15,
    ).stdout


def _certificates(directory):
    """Issue one ephemeral CA and purpose-constrained P-256 leaf pair."""
    paths = {}
    for name in ("ca", "server", "client"):
        key = directory / f"{name}-key.pem"
        cert = directory / f"{name}-cert.pem"
        _openssl("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", key)
        key.chmod(0o600)
        paths[f"{name}_key"] = key
        paths[f"{name}_cert"] = cert
        if name == "ca":
            _openssl(
                "req",
                "-x509",
                "-new",
                "-key",
                key,
                "-out",
                cert,
                "-days",
                "90",
                "-sha256",
                "-subj",
                "/CN=Relay integrity test CA",
                "-addext",
                "basicConstraints=critical,CA:TRUE,pathlen:0",
                "-addext",
                "keyUsage=critical,keyCertSign,cRLSign",
            )
            continue
        csr = directory / f"{name}.csr"
        extensions = directory / f"{name}.ext"
        purpose = "serverAuth" if name == "server" else "clientAuth"
        extension_text = (
            "basicConstraints=critical,CA:FALSE\n"
            "keyUsage=critical,digitalSignature\n"
            f"extendedKeyUsage={purpose}\n"
            "subjectKeyIdentifier=hash\n"
            "authorityKeyIdentifier=keyid,issuer\n"
        )
        if name == "server":
            extension_text += (
                "subjectAltName=IP:127.0.0.1,DNS:localhost,DNS:host.docker.internal\n"
            )
        extensions.write_text(extension_text, encoding="ascii")
        _openssl(
            "req",
            "-new",
            "-key",
            key,
            "-out",
            csr,
            "-subj",
            f"/CN=Relay integrity test {name}",
        )
        _openssl(
            "x509",
            "-req",
            "-in",
            csr,
            "-CA",
            paths["ca_cert"],
            "-CAkey",
            paths["ca_key"],
            "-set_serial",
            str(uuid4().int),
            "-out",
            cert,
            "-days",
            "90",
            "-sha256",
            "-extfile",
            extensions,
        )
    return paths


class TLSReceiver:
    """Capture-only SCP; assertions about integrity belong to the test caller.

    ``arrivals`` and ``streams`` return lock-protected snapshots. Each arrival's
    ``association_id`` indexes ``streams``. The full association objects are
    retained internally to prevent Python object-ID reuse between connections.
    ``expected_case`` is an optional test label, not a source of received bytes.
    """

    def __init__(self, directory, host="127.0.0.1"):
        self._paths = _certificates(Path(directory))
        self.ca_pem = self._paths["ca_cert"].read_text(encoding="ascii")
        self.client_key_pem = self._paths["client_key"].read_text(encoding="ascii")
        self.client_cert_pem = self._paths["client_cert"].read_text(encoding="ascii")
        self.server_cert_pem = self._paths["server_cert"].read_text(encoding="ascii")
        expiry = _openssl(
            "x509", "-in", self._paths["client_cert"], "-enddate", "-noout"
        )
        self.not_after = (
            datetime.fromtimestamp(
                ssl.cert_time_to_seconds(expiry.strip().split("=", 1)[1]), timezone.utc
            )
            .isoformat()
            .replace("+00:00", "Z")
        )
        self.client_fingerprint = sha256(
            ssl.PEM_cert_to_DER_cert(self.client_cert_pem)
        ).hexdigest()
        self.expected_case = None
        self.status = 0x0000
        self.hold_response = False
        self.response_entered = threading.Event()
        self.release_response = threading.Event()
        self._lock = threading.Lock()
        self._arrivals = []
        self._streams = {}

        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(self._paths["server_cert"], self._paths["server_key"])
        context.load_verify_locations(cadata=self.ca_pem)
        context.verify_mode = ssl.CERT_REQUIRED
        self._ae = AE(ae_title="INTEGRITY_SCP")
        self._ae.acse_timeout = 5
        self._ae.dimse_timeout = 15
        self._ae.network_timeout = 15
        for presentation_context in StoragePresentationContexts:
            self._ae.add_supported_context(
                presentation_context.abstract_syntax,
                [ExplicitVRLittleEndian, ImplicitVRLittleEndian, RLELossless],
            )
        self._server = self._ae.start_server(
            (host, 0),
            block=False,
            ssl_context=context,
            evt_handlers=[
                (evt.EVT_CONN_OPEN, self._connected),
                (evt.EVT_CONN_CLOSE, self._closed),
                (evt.EVT_DATA_RECV, self._received),
                (evt.EVT_DATA_SENT, self._sent),
                (evt.EVT_C_STORE, self._store),
            ],
        )
        self.port = self._server.server_address[1]

    def client_context(self, *, with_certificate=True):
        """Return a client that trusts only this receiver's ephemeral CA."""
        context = ssl.create_default_context(
            ssl.Purpose.SERVER_AUTH, cadata=self.ca_pem
        )
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        if with_certificate:
            context.load_cert_chain(
                self._paths["client_cert"], self._paths["client_key"]
            )
        return context

    @property
    def arrivals(self):
        with self._lock:
            return [
                dict(arrival, command=dict(arrival["command"]))
                for arrival in self._arrivals
            ]

    @property
    def streams(self):
        with self._lock:
            return {
                id(association): {
                    "received": bytes(capture["received"]),
                    "sent": bytes(capture["sent"]),
                    "peer_fingerprint": capture["peer_fingerprint"],
                    "closed": capture["closed"].is_set(),
                }
                for association, capture in self._streams.items()
            }

    def streams_for(self, arrival):
        return self.streams[arrival["association_id"]]

    def wait_for_closed(self, arrival, timeout=5):
        """Wait until both stream captures for an association are complete."""
        with self._lock:
            closed = next(
                capture["closed"]
                for association, capture in self._streams.items()
                if id(association) == arrival["association_id"]
            )
        return closed.wait(timeout)

    def _connected(self, event):
        certificate = event.assoc.dul.socket.socket.getpeercert(binary_form=True)
        with self._lock:
            self._streams[event.assoc] = {
                "received": bytearray(),
                "sent": bytearray(),
                "peer_fingerprint": sha256(certificate).hexdigest(),
                "closed": threading.Event(),
            }

    def _closed(self, event):
        with self._lock:
            capture = self._streams.get(event.assoc)
            if capture is not None:
                capture["closed"].set()

    def _received(self, event):
        with self._lock:
            self._streams[event.assoc]["received"].extend(event.data)

    def _sent(self, event):
        with self._lock:
            self._streams[event.assoc]["sent"].extend(event.data)

    def _store(self, event):
        request = event.request
        with self._lock:
            self._arrivals.append(
                {
                    "dataset": request.DataSet.getvalue(),
                    "transfer_syntax": str(event.context.transfer_syntax),
                    "command": {
                        "sop_class_uid": str(request.AffectedSOPClassUID),
                        "sop_instance_uid": str(request.AffectedSOPInstanceUID),
                        "message_id": request.MessageID,
                        "priority": request.Priority,
                    },
                    "peer_fingerprint": self._streams[event.assoc]["peer_fingerprint"],
                    "association_id": id(event.assoc),
                    "case": getattr(self.expected_case, "name", None),
                }
            )
        self.response_entered.set()
        if self.hold_response and not self.release_response.wait(15):
            return 0xC000
        return self.status

    def close(self):
        # Release a held C-STORE handler before joining the receiver's threads.
        self.release_response.set()
        self._ae.shutdown()


@contextmanager
def tls_receiver(directory, *, host="127.0.0.1"):
    """Start a receiver and remove its private keys when the context exits."""
    Path(directory).mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="dicom-tls-", dir=directory) as temporary:
        receiver = TLSReceiver(temporary, host=host)
        try:
            yield receiver
        finally:
            receiver.close()
