"""Certificate cleanup must work even when qualification fails."""

import ssl
import subprocess
import sys
from hashlib import sha1
from pathlib import Path

import pytest

from . import tls_trust


@pytest.fixture(scope="module")
def certificate(tmp_path_factory):
    directory = tmp_path_factory.mktemp("test-root")
    cert = directory / "cert.pem"
    subprocess.run(
        [
            "openssl",
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            str(directory / "key.pem"),
            "-out",
            str(cert),
            "-days",
            "1",
            "-subj",
            "/CN=Relay disposable trust test",
        ],
        check=True,
        timeout=30,
        capture_output=True,
    )
    return cert


@pytest.mark.parametrize("failure", [None, "body", "import", "cleanup"])
def test_windows_trust_removes_only_generated_certificate(
    certificate, monkeypatch, failure
):
    calls = []

    def run(args, **kwargs):
        assert kwargs["check"] and kwargs["timeout"] == 30
        calls.append(args)
        if (failure == "import" and "-addstore" in args) or (
            failure == "cleanup" and "-delstore" in args
        ):
            raise subprocess.CalledProcessError(1, args)

    monkeypatch.setattr(tls_trust.sys, "platform", "win32")
    monkeypatch.setenv("SystemRoot", "C:/Windows")
    monkeypatch.setattr(tls_trust.subprocess, "run", run)

    def exercise():
        with tls_trust.receiver_trust(certificate):
            if failure == "body":
                raise RuntimeError("synthetic comparison failure")

    if failure:
        with pytest.raises(
            RuntimeError if failure == "body" else subprocess.CalledProcessError
        ):
            exercise()
    else:
        exercise()
    thumbprint = sha1(ssl.PEM_cert_to_DER_cert(certificate.read_text())).hexdigest()
    assert calls == [
        [
            str(Path("C:/Windows/System32/certutil.exe")),
            "-addstore",
            "Root",
            str(certificate),
        ],
        [
            str(Path("C:/Windows/System32/certutil.exe")),
            "-delstore",
            "Root",
            thumbprint,
        ],
    ]


@pytest.mark.skipif(
    sys.platform != "win32", reason="requires Windows certificate store"
)
@pytest.mark.parametrize("fail_comparison", [False, True])
def test_native_windows_root_removed_after_qualification(certificate, fail_comparison):
    der = ssl.PEM_cert_to_DER_cert(certificate.read_text())

    def present():
        return any(data == der for data, _, _ in ssl.enum_certificates("ROOT"))

    assert not present()
    try:
        with tls_trust.receiver_trust(certificate):
            assert present()
            if fail_comparison:
                raise RuntimeError("synthetic comparison failure")
    except RuntimeError:
        if not fail_comparison:
            raise
    assert not present()


def test_ci_runs_native_windows_and_linux_with_separate_evidence():
    workflow = (
        Path(__file__).resolve().parents[3] / ".github/workflows/image-integrity.yml"
    ).read_text()
    for required in (
        "os: ubuntu-24.04",
        "os: windows-2025",
        "binary: telrad-relay.exe",
        "python: Scripts/python.exe",
        'python-version: "3.13.15"',
        "python-version: ${{ matrix.python-version }}",
        "runs-on: ${{ matrix.os }}",
        'go build -o "$RUNNER_TEMP/${{ matrix.binary }}"',
        '--relay-binary "$RUNNER_TEMP/${{ matrix.binary }}"',
        "-m pytest integration_tests",
        "-m integration_tests.relay_image_integrity",
        "target: linux",
        "target: windows",
        "target: docker",
        "--relay-image relay-integrity:current",
        "inputs.storage || 'local'",
        "allowed-account-ids: ${{ inputs.aws_account }}",
        "github.event_name == 'workflow_dispatch' && inputs.storage == 'aws'",
        "name: relay-image-integrity-${{ inputs.storage || 'local' }}-${{ matrix.target }}",
    ):
        assert required in workflow
