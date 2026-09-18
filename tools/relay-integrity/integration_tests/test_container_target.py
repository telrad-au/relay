"""The image target must retain packaging restrictions and always clean up."""

import json
from pathlib import Path
from types import SimpleNamespace

import pytest

from . import container_target as target


@pytest.mark.parametrize("failure", [None, "copy", "comparison"])
def test_image_is_pinned_restricted_and_removed(tmp_path, monkeypatch, failure):
    for name in ("relay.json", "relay-credential.json", "cert.pem"):
        (tmp_path / name).write_text("synthetic")
    calls = []
    owners = []

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
                            "Entrypoint": ["telrad-relay"],
                            "Cmd": ["run"],
                        },
                    }
                ]
            )
        if args[0] == "cp":
            if failure == "copy":
                raise RuntimeError("synthetic copy failure")
            Path(args[-1]).write_bytes(b"synthetic packaged executable")
        return ""

    monkeypatch.setattr(target, "docker", docker)
    monkeypatch.setattr(target, "owner", lambda *args: owners.append(args))
    monkeypatch.setattr(target.os, "getuid", lambda: 123, raising=False)
    monkeypatch.setattr(target.os, "getgid", lambda: 456, raising=False)
    monkeypatch.setattr(target.platform, "system", lambda: "Linux")
    monkeypatch.setattr(
        target.subprocess,
        "Popen",
        lambda *a, **kw: SimpleNamespace(wait=lambda **kw: 0),
    )
    report = {}

    def exercise():
        with target.packaged_process(
            "image:tag", tmp_path, tmp_path / "cert.pem", report
        ):
            if failure == "comparison":
                raise RuntimeError("synthetic comparison failure")

    if failure:
        with pytest.raises(RuntimeError):
            exercise()
    else:
        exercise()
    create = next(c for c in calls if c[0] == "create")
    assert create[-1] == "sha256:synthetic"
    assert "--read-only" in create and "ALL" in create
    assert "no-new-privileges:true" in create
    assert "--entrypoint" not in create and "--user" not in create
    assert [c[0] for c in calls[-2:]] == ["stop", "rm"]
    assert calls[-2][1:3] == ("-t", "5")
    assert owners[0][1:] == (10001, 10001) and owners[-1][1:] == (123, 456)
    assert report["container"]["cleanup"] == "passed"
