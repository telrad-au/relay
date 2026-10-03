"""The image target must retain packaging restrictions and always clean up."""

import json
import os
from pathlib import Path
from types import SimpleNamespace

import pytest

from . import container_target as target
from .image_integrity import IntegrityFailure


@pytest.mark.parametrize(
    "failure", [None, "create", "copy", "start", "comparison", "stop", "rm", "wait"]
)
def test_image_is_pinned_restricted_and_removed(tmp_path, monkeypatch, failure):
    config = {"schemaVersion": 6, "dataDir": "."}
    identity = {"schemaVersion": 1, "privateKey": "synthetic fixture only"}
    (tmp_path / "relay.json").write_text(json.dumps(config))
    (tmp_path / "identity.json").write_text(json.dumps(identity))
    # The CA signing key and server key must never enter the Relay container.
    for name in ("ca-key.pem", "server-key.pem", "unrelated.txt"):
        (tmp_path / name).write_text("synthetic")
    calls = []
    owners = []
    waits = []

    def docker(*args):
        calls.append(args)
        if args[:2] == ("image", "inspect"):
            return json.dumps(
                [
                    {
                        "Id": "sha256:synthetic",
                        "Os": "linux",
                        "Config": {
                            "User": "10001:10001",
                            "Entrypoint": [
                                "telrad-relay",
                                "--config",
                                "/var/lib/telrad-relay/relay.json",
                            ],
                            "Cmd": ["run"],
                        },
                    }
                ]
            )
        if args[0] == failure or (args[0] == "cp" and failure == "copy"):
            raise RuntimeError("synthetic Docker failure")
        if args[0] == "cp":
            Path(args[-1]).write_bytes(b"synthetic packaged executable")
        return ""

    def wait(**kwargs):
        waits.append(kwargs)
        if failure == "wait":
            raise RuntimeError("synthetic wait failure")
        return 0

    def start(*args, **kwargs):
        if failure == "start":
            raise RuntimeError("synthetic start failure")
        assert args[0][:3] == ["docker", "start", "--attach"]
        return SimpleNamespace(wait=wait)

    monkeypatch.setattr(target, "docker", docker)
    monkeypatch.setattr(target, "owner", lambda *args: owners.append(args))
    monkeypatch.setattr(target.os, "getuid", lambda: 123, raising=False)
    monkeypatch.setattr(target.os, "getgid", lambda: 456, raising=False)
    monkeypatch.setattr(target.platform, "system", lambda: "Linux")
    monkeypatch.setattr(target.subprocess, "Popen", start)
    report = {}

    def exercise():
        with target.packaged_process("image:tag", tmp_path, report):
            if failure == "comparison":
                raise RuntimeError("synthetic comparison failure")

    if failure:
        with pytest.raises(RuntimeError, match="synthetic"):
            exercise()
    else:
        exercise()
    create = next(c for c in calls if c[0] == "create")
    assert create[-1] == "sha256:synthetic"
    assert create[create.index("--network") + 1] == "host"
    assert "--read-only" in create and "ALL" in create
    assert "no-new-privileges:true" in create
    assert "--entrypoint" not in create and "--user" not in create
    assert "--env" not in create
    assert create[create.index("--mount") + 1] == (
        f"type=bind,src={tmp_path / 'container-state'},dst=/var/lib/telrad-relay"
    )
    state = tmp_path / "container-state"
    assert sorted(p.name for p in state.iterdir()) == ["identity.json", "relay.json"]
    assert json.loads((state / "relay.json").read_text()) == config
    assert json.loads((state / "identity.json").read_text()) == identity
    if os.name == "posix":
        assert state.stat().st_mode & 0o777 == 0o700
        assert all(p.stat().st_mode & 0o777 == 0o600 for p in state.iterdir())
    assert owners[0][1:] == (10001, 10001) and owners[-1][1:] == (123, 456)
    if failure == "create":
        assert not any(c[0] in ("stop", "rm") for c in calls)
        assert "container" not in report
    else:
        assert [c[0] for c in calls[-2:]] == ["stop", "rm"]
        assert calls[-2][1:3] == ("-t", "5")
        if failure in ("stop", "rm"):
            assert "cleanup" not in report.get("container", {})
        else:
            assert report["container"]["cleanup"] == "passed"
    if failure not in ("create", "copy", "start"):
        assert waits == [{"timeout": 10}]


@pytest.mark.parametrize(
    "system,image_os,user,error",
    [
        ("Windows", "linux", "10001:10001", "requires a Linux Docker host"),
        ("Linux", "windows", "10001:10001", "expected Linux image"),
        ("Linux", "linux", "0:0", "expected packaged non-root user"),
    ],
)
def test_unsupported_or_privileged_image_fails_before_state_creation(
    tmp_path, monkeypatch, system, image_os, user, error
):
    calls = []

    def docker(*args):
        calls.append(args)
        assert args == ("image", "inspect", "image:tag")
        return json.dumps([{"Os": image_os, "Config": {"User": user}}])

    monkeypatch.setattr(target.platform, "system", lambda: system)
    monkeypatch.setattr(target, "docker", docker)
    with (
        pytest.raises(IntegrityFailure, match=error),
        target.packaged_process("image:tag", tmp_path, {}),
    ):
        pytest.fail("unsupported image was started")
    assert not (tmp_path / "container-state").exists()
    assert len(calls) == (0 if system != "Linux" else 1)
