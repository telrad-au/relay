"""Run the release image on a disposable Linux Docker host."""

from contextlib import contextmanager
import json
import os
import platform
import shutil
import subprocess
from uuid import uuid4

from .image_integrity import digest, require


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True, timeout=60)


def owner(path, uid, gid):
    if os.geteuid() == 0:
        for child in path.iterdir():
            os.chown(child, uid, gid)
        os.chown(path, uid, gid)
    else:
        subprocess.run(
            ["sudo", "-n", "chown", "-R", f"{uid}:{gid}", str(path)],
            check=True,
            timeout=15,
        )


@contextmanager
def packaged_process(image, directory, cert, report):
    require(platform.system() == "Linux", "packaged image requires a Linux Docker host")
    details = json.loads(docker("image", "inspect", image))[0]
    require(details["Os"] == "linux", "expected Linux image")
    require(
        details["Config"]["User"] == "10001:10001", "expected packaged non-root user"
    )
    name = "relay-integrity-" + uuid4().hex
    state = directory / "container-state"
    state.mkdir(mode=0o700)
    for filename in ("relay.json", "relay-credential.json"):
        shutil.copyfile(directory / filename, state / filename)
        (state / filename).chmod(0o600)
    shutil.copyfile(cert, state / "cert.pem")
    created = False
    process = None
    try:
        owner(state, 10001, 10001)
        docker(
            "create",
            "--name",
            name,
            "--network",
            "host",
            "--read-only",
            "--cap-drop",
            "ALL",
            "--security-opt",
            "no-new-privileges:true",
            "--mount",
            f"type=bind,src={state},dst=/var/lib/telrad-relay",
            "--env",
            "SSL_CERT_FILE=/var/lib/telrad-relay/cert.pem",
            details["Id"],
        )
        created = True
        binary = directory / "packaged-relay"
        docker("cp", f"{name}:/usr/local/bin/telrad-relay", str(binary))
        report["relayBinarySha256"] = digest(binary.read_bytes())
        report["container"] = {
            "imageId": details["Id"],
            "user": details["Config"]["User"],
            "entrypoint": details["Config"]["Entrypoint"],
            "command": details["Config"]["Cmd"],
            "readOnlyRootFilesystem": True,
            "capDrop": ["ALL"],
            "noNewPrivileges": True,
        }
        process = subprocess.Popen(
            ["docker", "start", "--attach", name],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        yield process
    finally:
        try:
            if created:
                docker("stop", "--timeout", "5", name)
                docker("rm", name)
                report.setdefault("container", {})["cleanup"] = "passed"
        finally:
            try:
                if process is not None:
                    process.wait(timeout=10)
            finally:
                owner(state, os.getuid(), os.getgid())
