"""Runs on the disposable sender; never runs workload traffic on the laptop."""

import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import tarfile
import time

GO_IMAGE = "golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc"
BASE = Path("/opt/relay-perf")


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--relay", required=True)
    parser.add_argument("--bucket", required=True)
    args = parser.parse_args()
    cfg = json.loads((BASE / "run.json").read_text())
    out = BASE / "results"
    out.mkdir(exist_ok=True)
    phase = "setup"
    summary = {"status": "ERROR", "retrySender": False}

    def status(value):
        data = {
            "phase": value,
            "at": int(time.time()),
            "sender": cfg["sender"],
            "relay": cfg["relay"],
        }
        (out / "status.json").write_text(json.dumps(data))
        run(
            [
                "aws",
                "s3",
                "cp",
                "--only-show-errors",
                str(out / "status.json"),
                f"s3://{args.bucket}/results/status.json",
            ]
        )

    try:
        status(phase)
        for attempt in range(120):
            if (
                subprocess.run(
                    [
                        "ssh",
                        "-o",
                        "ConnectTimeout=5",
                        f"ec2-user@{args.relay}",
                        'test "$(wc -l < /proc/swaps)" -eq 1 && docker info --format {{.ServerVersion}}',
                    ],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                ).returncode
                == 0
            ):
                break
            time.sleep(5)
        else:
            raise RuntimeError("relay_bootstrap_unavailable")
        run(["git", "init", "-q"])
        run(
            [
                "git",
                "fetch",
                "-q",
                "--depth=1",
                "https://github.com/telrad-au/relay.git",
                cfg["revision"],
            ]
        )
        run(["git", "reset", "-q", "--mixed", "FETCH_HEAD"])
        toolchain = BASE / "toolchain"
        toolchain.mkdir(exist_ok=True)
        run(
            [
                "docker",
                "run",
                "--rm",
                "--entrypoint",
                "sh",
                "--mount",
                f"type=bind,src={toolchain},dst=/out",
                GO_IMAGE,
                "-c",
                "cp -a /usr/local/go /out/go",
            ]
        )
        os.environ["PATH"] = str(toolchain / "go/bin") + ":" + os.environ["PATH"]
        os.environ["GOCACHE"] = str(BASE / "go-cache")
        os.environ["GOPATH"] = str(BASE / "go")
        # AL2023 mounts /tmp in RAM. Builds and multi-GiB fixtures belong on EBS.
        os.environ["TMPDIR"] = "/var/tmp"
        # Compile and check on AWS before any measured traffic. Serial builds
        # reduce setup memory without constraining the measured release binary.
        phase = "validation"
        status(phase)
        run(["dnf", "install", "-y", "gcc"])
        env = dict(os.environ, GOMAXPROCS="1", GOFLAGS="-p=1", GOMEMLIMIT="256MiB")
        for command in [
            ["go", "test", "-race", "./..."],
            ["go", "vet", "./..."],
            ["scripts/check-publication.sh"],
            ["scripts/check-licenses.sh"],
        ]:
            run(command, env=env)
        run(
            [
                "go",
                "build",
                "-p=1",
                "-o",
                str(BASE / "relay-perf"),
                "./tools/relay-perf",
            ],
            env=env,
        )
        run(
            [
                "go",
                "test",
                "-race",
                "./cmd/telrad-relay",
                "-run",
                "^TestOrthancDICOMPayloadIntegrity$",
                "-count=1",
                "-timeout=5m",
            ],
            env=dict(env, TELRAD_ORTHANC_INTEROP_TEST="1"),
        )
        run(["scripts/check-hl7-fixtures.sh"])
        run(
            [
                "go",
                "test",
                "./cmd/telrad-relay",
                "-run",
                "^$",
                "-bench",
                "^(BenchmarkReadMLLPFrame|BenchmarkDICOMParsing|BenchmarkDICOMStream|BenchmarkHL7PersistentMixed|BenchmarkHL7Stream)$",
                "-benchtime=1x",
                "-benchmem",
            ],
            env=env,
        )
        run(["go", "run", "golang.org/x/vuln/cmd/govulncheck@v1.7.0", "./..."], env=env)
        phase = "screen"
        status(phase)
        sender = socket.gethostbyname(socket.gethostname())
        code = subprocess.run(
            [
                str(BASE / "relay-perf"),
                "screen",
                "--profile",
                cfg["workload"],
                "--cpus",
                str(cfg["cpus"]),
                "--memory",
                cfg["memory"],
                "--relay-docker",
                f"ssh://ec2-user@{args.relay}",
                "--relay-host",
                args.relay,
                "--support-host",
                sender,
                "--out",
                str(out / "performance"),
            ]
        ).returncode
        result = out / "performance/point-1-run-1/result.json"
        if result.exists():
            measured = json.loads(result.read_text())
            reasons = measured.get("reasons") or []
            summary = {
                "status": measured["status"],
                "reasons": reasons,
                "exitCode": code,
                "retrySender": any(
                    "support" in reason or "calibration" in reason for reason in reasons
                ),
            }
        else:
            summary = {
                "status": "ERROR",
                "reason": "missing_harness_result",
                "exitCode": code,
                "retrySender": False,
            }
    except Exception as exc:
        # Retain diagnostics but do not label a provisioning/auth/code failure as
        # evidence that a larger sender is needed. Only OOM supports that retry.
        summary = {
            "status": "ERROR",
            "phase": phase,
            "errorType": type(exc).__name__,
            "retrySender": False,
        }
    finally:
        # A killed harness can return a nonzero exit without raising above.
        # Preserve sender OOM evidence on that path as well as setup exceptions.
        oom = subprocess.run(
            ["sh", "-c", "dmesg | grep -Ei 'out of memory|oom-kill|killed process'"],
            capture_output=True,
            text=True,
        ).stdout
        (out / "oom.log").write_text(oom)
        if oom:
            summary["retrySender"] = True
            summary["senderOOM"] = True
            if summary["status"] == "PASS":
                summary["status"] = "INCONCLUSIVE"
        (out / "summary.json").write_text(json.dumps(summary, indent=2))
        (out / "run.json").write_text(json.dumps(cfg, indent=2))
        log = BASE / "controller.log"
        if log.exists():
            (out / "controller.log").write_bytes(log.read_bytes())
        archive = BASE / "results.tar.gz"
        with tarfile.open(archive, "w:gz") as tar:
            tar.add(out, arcname="results")
        run(
            [
                "aws",
                "s3",
                "cp",
                "--only-show-errors",
                str(archive),
                f"s3://{args.bucket}/results/results.tar.gz",
            ]
        )
        run(
            [
                "aws",
                "s3",
                "cp",
                "--only-show-errors",
                str(out / "summary.json"),
                f"s3://{args.bucket}/results/summary.json",
            ]
        )
        status("complete")


if __name__ == "__main__":
    main()
