"""Temporary certificate trust for disposable Windows qualification hosts."""

import os
import ssl
import subprocess
import sys
from contextlib import contextmanager
from hashlib import sha1
from pathlib import Path


@contextmanager
def receiver_trust(cert):
    if sys.platform != "win32":
        yield
        return
    # Go uses the Windows certificate store, not SSL_CERT_FILE. CurrentUser Root
    # imports prompt on headless hosts. Like Relay's native lifecycle tests, use
    # the machine store on an administrator-owned disposable test host only.
    certificate = ssl.PEM_cert_to_DER_cert(cert.read_text())
    thumbprint = sha1(certificate).hexdigest()
    certutil = str(Path(os.environ["SystemRoot"]) / "System32" / "certutil.exe")

    def run(*args):
        subprocess.run(
            [certutil, *args],
            check=True,
            timeout=30,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    try:
        run("-addstore", "Root", str(cert))
        yield
    finally:
        # Also attempt cleanup when import fails or times out after a write.
        # A removal failure fails qualification rather than claiming cleanup.
        run("-delstore", "Root", thumbprint)
