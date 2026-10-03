"""Keep the dedicated CI matrix and its test-only dependency contract explicit."""

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]


def test_ci_runs_native_windows_linux_and_release_image_with_separate_evidence():
    workflow = (ROOT / ".github/workflows/image-integrity.yml").read_text()
    for required in (
        "os: ubuntu-24.04",
        "os: windows-2025",
        "binary: telrad-relay.exe",
        "python: Scripts/python.exe",
        'python-version: "3.13.15"',
        "python-version: ${{ matrix.python-version }}",
        "runs-on: ${{ matrix.os }}",
        'go-version: "1.27.0"',
        'go build -o "$RUNNER_TEMP/${{ matrix.binary }}"',
        '--relay-binary "$RUNNER_TEMP/${{ matrix.binary }}"',
        "-m pip install --require-hashes -r tools/relay-integrity/requirements.lock",
        "openssl version",
        "-m pytest integration_tests",
        "-m integration_tests.relay_image_integrity",
        "target: linux",
        "target: windows",
        "target: docker",
        'docker build --build-arg VERSION=0.0.0-integrity --build-arg REVISION="$(git rev-parse HEAD)" -t relay-integrity:current .',
        "--relay-image relay-integrity:current",
        "name: relay-image-integrity-${{ matrix.target }}",
        "if: always()",
        "contents: read",
        "persist-credentials: false",
        "ref: ${{ github.event.pull_request.head.sha || github.sha }}",
    ):
        assert required in workflow
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    checkout = re.search(r"actions/checkout@[0-9a-f]{40}", ci).group()
    assert checkout in workflow
    assert "id-token: write" not in workflow
    assert "aws" not in workflow.lower()
    assert "certutil" not in workflow.lower()
    assert "SSL_CERT_FILE" not in workflow


def test_only_dicom_pixel_and_pytest_dependencies_are_locked():
    inputs = (ROOT / "tools/relay-integrity/requirements.in").read_text()
    lock = (ROOT / "tools/relay-integrity/requirements.lock").read_text()
    pins = dict(re.findall(r"^([a-z][a-z0-9-]*)==([^\s]+)", inputs, re.MULTILINE))
    assert pins == {
        "colorama": "0.4.6",  # Required by pytest on Windows.
        "numpy": "2.4.2",
        "pydicom": "3.0.2",
        "pynetdicom": "3.0.4",
        "pytest": "8.4.2",
    }
    locked = dict(re.findall(r"^([a-z][a-z0-9-]*)==([^\s]+)", lock, re.MULTILINE))
    assert set(locked) == set(pins) | {"iniconfig", "packaging", "pluggy", "pygments"}
    assert all(locked[name] == version for name, version in pins.items())
    blocks = re.split(r"\n(?=[a-z][a-z0-9-]*==)", lock)
    for block in blocks[1:]:
        assert re.search(r"--hash=sha256:[0-9a-f]{64}(?:\s|$)", block)


def test_fixture_uses_current_config_schema_and_pinned_identity(tmp_path, monkeypatch):
    import json
    import os
    from types import SimpleNamespace

    from . import relay_image_integrity as runner

    receiver = SimpleNamespace(
        port=40100,
        client_key_pem="synthetic client key",
        client_cert_pem="synthetic client certificate",
        ca_pem="synthetic pinned CA",
        not_after="2030-01-01T00:00:00Z",
    )
    monkeypatch.setattr(
        runner, "free_ports", lambda count: [40101, 40102, 40103, 40104]
    )
    config = runner.write_configuration(tmp_path, receiver)
    source = (ROOT / "cmd/telrad-relay/config.go").read_text()
    current_schema = int(
        re.search(r"currentConfigSchemaVersion\s*=\s*(\d+)", source)[1]
    )
    known_fields = set(re.findall(r'`json:"([^",]+)', source))
    assert config["schemaVersion"] == current_schema == 6
    assert set(config) <= known_fields
    assert config["dataDir"] == "."
    assert config["listenAddress"] == "127.0.0.1"
    assert config["statusAddress"] == "127.0.0.1:40103"
    assert json.loads((tmp_path / "relay.json").read_text()) == config
    identity = json.loads((tmp_path / "identity.json").read_text())
    assert identity["schemaVersion"] == 1
    assert identity["privateKey"] == receiver.client_key_pem
    assert identity["certificate"] == receiver.client_cert_pem
    assert identity["notAfter"] == receiver.not_after
    assert identity["telrad"] == {
        "host": "127.0.0.1",
        "dicomPort": receiver.port,
        "hl7Port": 40104,
        "reportPort": 40104,
        "caCertificate": receiver.ca_pem,
    }
    assert sorted(p.name for p in tmp_path.iterdir()) == ["identity.json", "relay.json"]
    if os.name == "posix":
        assert all(p.stat().st_mode & 0o777 == 0o600 for p in tmp_path.iterdir())


def test_fixture_port_allocation_is_distinct():
    from .relay_image_integrity import free_ports

    ports = free_ports(10)
    assert len(set(ports)) == 10
    assert all(0 < port < 65536 for port in ports)
