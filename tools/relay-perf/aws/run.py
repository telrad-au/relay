#!/usr/bin/env python3
"""One-command, isolated AWS screening. Uses the caller's normal AWS CLI chain."""

import argparse
import datetime as dt
import hashlib
import json
import os
import re
from pathlib import Path
import secrets
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
SENDERS = ["t3a.micro", "t3a.small", "c6a.large", "c6a.xlarge"]


def invoke(args, capture=True, check=True, **kwargs):
    return subprocess.run(
        args,
        check=check,
        text=True,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None,
        **kwargs,
    )


class AWS:
    def __init__(self, region, profile=None):
        self.prefix = ["aws", "--no-cli-pager", "--region", region]
        if profile:
            self.prefix += ["--profile", profile]

    def call(self, *args, **kwargs):
        result = invoke(self.prefix + list(args), **kwargs)
        return json.loads(result.stdout) if result.stdout.strip() else {}

    def copy(self, source, destination):
        invoke(
            self.prefix
            + ["s3", "cp", "--only-show-errors", str(source), str(destination)]
        )


def source_bundle(destination):
    paths = (
        subprocess.check_output(
            ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
            cwd=ROOT,
        )
        .decode()
        .split("\0")
    )
    roots = {"cmd", "internal", "packaging", "scripts", "tools", "docs", ".github"}
    files = {
        "AGENTS.md",
        "README.md",
        "SECURITY.md",
        "LICENSE",
        "NOTICE",
        "THIRD_PARTY_NOTICES.md",
        "Dockerfile",
        ".dockerignore",
        ".gitignore",
        "go.mod",
        "go.sum",
    }
    with tarfile.open(destination, "w:gz") as tar:
        for name in sorted(set(paths)):
            path = Path(name)
            if not name or (path.parts[0] not in roots and name not in files):
                continue
            if any(
                part in {"node_modules", "cdk.out", "__pycache__", ".git"}
                for part in path.parts
            ):
                continue
            if path.name.startswith(".env") or path.suffix.lower() in {
                ".key",
                ".pem",
                ".pfx",
                ".p12",
            }:
                raise ValueError(
                    f"Private or certificate file is outside the source bundle contract: {name}"
                )
            source = ROOT / path
            if source.is_symlink() or not source.resolve().is_relative_to(ROOT):
                raise ValueError("Source bundles must not contain symlinks")
            if source.is_file():
                if source.stat().st_size > 10 << 20:
                    raise ValueError(f"Unexpected large source file: {name}")
                if name != "scripts/check-publication.sh" and re.search(
                    rb"BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9_]{20,}",
                    source.read_bytes(),
                ):
                    raise ValueError(f"Potential secret in source bundle: {name}")
                tar.add(source, arcname=name, recursive=False)
    with open(destination, "rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def unpack_evidence(archive, destination):
    destination = destination.resolve()
    with tarfile.open(archive) as tar:
        members = tar.getmembers()
        if sum(member.size for member in members) > 1 << 30:
            raise ValueError("Evidence exceeds the 1 GiB bound")
        for member in members:
            target = (destination / member.name).resolve()
            if not target.is_relative_to(destination) or not (
                member.isfile() or member.isdir()
            ):
                raise ValueError("Unsafe evidence archive entry")
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with tar.extractfile(member) as source, target.open("wb") as output:
                    shutil.copyfileobj(source, output)


def stack_state(aws, name):
    try:
        return aws.call("cloudformation", "describe-stacks", "--stack-name", name)[
            "Stacks"
        ][0]
    except subprocess.CalledProcessError as exc:
        if "does not exist" in (exc.stderr or ""):
            return {"StackStatus": "DELETE_COMPLETE"}
        raise


def wait_stack(aws, name, wanted, deadline):
    while time.time() < deadline:
        state = stack_state(aws, name)
        status = state["StackStatus"]
        if status == wanted:
            return {
                item["OutputKey"]: item["OutputValue"]
                for item in state.get("Outputs", [])
            }
        if status.endswith("_FAILED") or "ROLLBACK" in status:
            raise RuntimeError(f"{name}: {status}")
        time.sleep(5)
    raise TimeoutError(f"{name}: stack operation timed out")


def destroy(aws, cfg, out):
    deadline = time.time() + 600
    cleanup = {"complete": False, "watchdogExpires": cfg["expires"]}
    try:
        while time.time() < deadline:
            if stack_state(aws, cfg["run"])["StackStatus"] == "DELETE_COMPLETE":
                cleanup["complete"] = True
                break
            response = out / "cleanup-response.json"
            try:
                aws.call(
                    "lambda",
                    "invoke",
                    "--function-name",
                    cfg["run"] + "-cleanup",
                    "--cli-binary-format",
                    "raw-in-base64-out",
                    "--payload",
                    '{"cleanup":true}',
                    str(response),
                )
            except subprocess.CalledProcessError as exc:
                state = stack_state(aws, cfg["run"])["StackStatus"]
                if state not in {"DELETE_IN_PROGRESS", "DELETE_COMPLETE"}:
                    if (
                        "ResourceNotFoundException" not in (exc.stderr or "")
                        or stack_state(aws, cfg["run"] + "-workload")["StackStatus"]
                        != "DELETE_COMPLETE"
                    ):
                        raise
                    # A failed guard deployment may have no Lambda yet. There
                    # cannot be any compute before the guard finishes creation.
                    aws.call(
                        "cloudformation", "delete-stack", "--stack-name", cfg["run"]
                    )
            time.sleep(10)
        if not cleanup["complete"]:
            raise TimeoutError(
                "AWS teardown has not completed; expiry watchdog remains responsible"
            )
        # A deleted stack is insufficient if anything was unexpectedly retained.
        reservations = aws.call(
            "ec2",
            "describe-instances",
            "--filters",
            f'Name=tag:RelayPerfRun,Values={cfg["run"]}',
            "Name=instance-state-name,Values=pending,running,stopping,stopped",
        )["Reservations"]
        volumes = aws.call(
            "ec2",
            "describe-volumes",
            "--filters",
            f'Name=tag:RelayPerfRun,Values={cfg["run"]}',
        )["Volumes"]
        vpcs = aws.call(
            "ec2",
            "describe-vpcs",
            "--filters",
            f'Name=tag:RelayPerfRun,Values={cfg["run"]}',
        )["Vpcs"]
        if (
            reservations
            or volumes
            or vpcs
            or stack_state(aws, cfg["run"] + "-workload")["StackStatus"]
            != "DELETE_COMPLETE"
        ):
            cleanup["complete"] = False
            raise RuntimeError(
                "Run-owned compute, network or workload stack remains after deletion"
            )
        cleanup["audit"] = {
            "activeInstances": 0,
            "volumes": 0,
            "vpcs": 0,
            "guardStack": "deleted",
            "workloadStack": "deleted",
        }
    except Exception as exc:
        cleanup["errorType"] = type(exc).__name__
        raise
    finally:
        (out / "cleanup.json").write_text(json.dumps(cleanup, indent=2))


def execute(aws, base, sender, out):
    run_id = "relay-perf-" + secrets.token_hex(6)
    out.mkdir(parents=True)
    cfg = dict(
        base,
        run=run_id,
        sender=sender,
        expires=int(time.time()) + base["ttlMinutes"] * 60,
    )
    cfg.pop("sourceArchive", None)
    (out / "run.json").write_text(json.dumps(cfg, indent=2))
    synthesized = invoke(
        ["node", str(HERE / "stack.mjs"), str(out / "run.json"), str(out / "cdk.out")],
        cwd=ROOT,
    )
    templates = json.loads(synthesized.stdout.strip().splitlines()[-1])
    for path in templates.values():
        aws.call(
            "cloudformation", "validate-template", "--template-body", "file://" + path
        )
    guard_created = False
    try:
        print(f'Creating {run_id}: Relay {cfg["relay"]}; sender {sender}', flush=True)
        aws.call(
            "cloudformation",
            "create-stack",
            "--stack-name",
            run_id,
            "--template-body",
            "file://" + templates["guard"],
            "--capabilities",
            "CAPABILITY_NAMED_IAM",
            "--tags",
            f"Key=RelayPerfRun,Value={run_id}",
        )
        guard_created = True
        outputs = wait_stack(aws, run_id, "CREATE_COMPLETE", time.time() + 900)
        (out / "outputs.json").write_text(json.dumps(outputs, indent=2))
        with tempfile.TemporaryDirectory(prefix="relay-perf-keys-") as private:
            private = Path(private)
            for key in ["sender_key", "relay_host_key"]:
                invoke(
                    [
                        "ssh-keygen",
                        "-q",
                        "-t",
                        "ed25519",
                        "-N",
                        "",
                        "-f",
                        str(private / key),
                    ]
                )
                for suffix in ["", ".pub"]:
                    aws.copy(
                        private / (key + suffix),
                        f's3://{outputs["Bucket"]}/private/{key+suffix}',
                    )
            aws.copy(base["sourceArchive"], f's3://{outputs["Bucket"]}/source.tar.gz')
            aws.copy(out / "run.json", f's3://{outputs["Bucket"]}/run.json')
        aws.call(
            "cloudformation",
            "create-stack",
            "--stack-name",
            run_id + "-workload",
            "--template-body",
            "file://" + templates["workload"],
            "--tags",
            f"Key=RelayPerfRun,Value={run_id}",
        )
        outputs.update(
            wait_stack(aws, run_id + "-workload", "CREATE_COMPLETE", time.time() + 900)
        )
        (out / "outputs.json").write_text(json.dumps(outputs, indent=2))
        last_phase = None
        while time.time() < cfg["expires"] - 120:
            try:
                aws.copy(
                    f's3://{outputs["Bucket"]}/results/status.json', out / "status.json"
                )
                phase = json.loads((out / "status.json").read_text())["phase"]
                if phase != last_phase:
                    print(f"{run_id}: {phase}", flush=True)
                    last_phase = phase
            except subprocess.CalledProcessError as exc:
                if "404" not in (exc.stderr or "") and "NoSuchKey" not in (
                    exc.stderr or ""
                ):
                    raise
            try:
                aws.copy(
                    f's3://{outputs["Bucket"]}/results/summary.json',
                    out / "summary.json",
                )
                aws.copy(
                    f's3://{outputs["Bucket"]}/results/results.tar.gz',
                    out / "results.tar.gz",
                )
                unpack_evidence(out / "results.tar.gz", out)
                break
            except subprocess.CalledProcessError as exc:
                if "404" not in (exc.stderr or "") and "NoSuchKey" not in (
                    exc.stderr or ""
                ):
                    raise
                time.sleep(15)
        else:
            raise TimeoutError(
                "Run reached the cleanup deadline without final evidence"
            )
        inventory = aws.call(
            "ec2",
            "describe-instances",
            "--instance-ids",
            outputs["Relay"],
            outputs["Sender"],
        )
        (out / "instances.json").write_text(json.dumps(inventory, indent=2))
        # The watchdog may already have terminated compute after final upload.
        # Capture configured credit mode from the resolved template as well.
        (out / "credit-mode.json").write_text(
            json.dumps(
                {
                    role: "unlimited" if cfg[key].startswith("t3") else "not-burstable"
                    for role, key in [("Relay", "relay"), ("Sender", "sender")]
                },
                indent=2,
            )
        )
        metrics = {}
        for role in ["Relay", "Sender"]:
            for metric in [
                "CPUCreditBalance",
                "CPUSurplusCreditBalance",
                "CPUSurplusCreditsCharged",
            ]:
                metrics[f"{role}/{metric}"] = aws.call(
                    "cloudwatch",
                    "get-metric-statistics",
                    "--namespace",
                    "AWS/EC2",
                    "--metric-name",
                    metric,
                    "--dimensions",
                    f"Name=InstanceId,Value={outputs[role]}",
                    "--start-time",
                    dt.datetime.fromtimestamp(
                        cfg["expires"] - cfg["ttlMinutes"] * 60, dt.timezone.utc
                    ).isoformat(),
                    "--end-time",
                    dt.datetime.now(dt.timezone.utc).isoformat(),
                    "--period",
                    "60",
                    "--statistics",
                    "Maximum",
                )
        (out / "cpu-credits.json").write_text(json.dumps(metrics, indent=2))
        return json.loads((out / "summary.json").read_text())
    except BaseException as exc:
        (out / "controller-error.json").write_text(
            json.dumps(
                {
                    "status": (
                        "CANCELLED" if isinstance(exc, KeyboardInterrupt) else "ERROR"
                    ),
                    "errorType": type(exc).__name__,
                    "at": int(time.time()),
                },
                indent=2,
            )
        )
        raise
    finally:
        if guard_created:
            print(f"{run_id}: tearing down owned resources", flush=True)
            destroy(aws, cfg, out)


def main():
    def interrupted(_signum, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--aws-profile")
    parser.add_argument("--region")
    parser.add_argument(
        "--relay-instance",
        choices=["t3a.nano", "t3a.micro", "t3a.small"],
        default="t3a.nano",
    )
    parser.add_argument(
        "--sender-instance",
        choices=SENDERS,
        help="Disable automatic sender selection and use this candidate",
    )
    parser.add_argument(
        "--workload",
        choices=["clinic-mixed-v1", "clinic-v1"],
        default="clinic-mixed-v1",
    )
    parser.add_argument("--cpus", type=float, default=1)
    parser.add_argument("--memory", default="256MiB")
    parser.add_argument("--ttl-minutes", type=int, default=90)
    parser.add_argument(
        "--out",
        default="dist/perf-aws-"
        + dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ"),
    )
    args = parser.parse_args()
    if not 30 <= args.ttl_minutes <= 180:
        parser.error("ttl-minutes must be between 30 and 180")
    region = (
        args.region
        or os.environ.get("AWS_REGION")
        or os.environ.get("AWS_DEFAULT_REGION")
    )
    if not region:
        command = (
            ["aws"]
            + (["--profile", args.aws_profile] if args.aws_profile else [])
            + ["configure", "get", "region"]
        )
        region = invoke(command).stdout.strip()
    aws = AWS(region, args.aws_profile)
    identity = aws.call("sts", "get-caller-identity")
    out = (ROOT / args.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    print(
        f'AWS account {identity["Account"]}, region {region}; no local Docker or load generation',
        flush=True,
    )
    if not (HERE / "node_modules/aws-cdk-lib/package.json").exists():
        invoke(["npm", "ci", "--ignore-scripts", "--no-fund"], cwd=HERE, capture=False)
    invoke(["npm", "test"], cwd=HERE, capture=False)
    invoke(
        ["python3", "-m", "unittest", "discover", "-s", str(HERE), "-p", "test_*.py"],
        capture=False,
    )
    ami = aws.call(
        "ssm",
        "get-parameter",
        "--name",
        "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64",
    )["Parameter"]["Value"]
    # Smaller candidates remain reproducible, but did not establish supporting
    # headroom in the recorded mixed-study screening experiments.
    candidates = [args.sender_instance] if args.sender_instance else SENDERS[2:]
    archive = out / "source.tar.gz"
    digest = source_bundle(archive)
    revision = invoke(["git", "rev-parse", "HEAD"], cwd=ROOT).stdout.strip()
    base = {
        "account": identity["Account"],
        "region": region,
        "ami": ami,
        "relay": args.relay_instance,
        "workload": args.workload,
        "cpus": args.cpus,
        "memory": args.memory,
        "ttlMinutes": args.ttl_minutes,
        "revision": revision,
        "sourceSHA256": digest,
        "sourceArchive": str(archive),
    }
    for index, sender in enumerate(candidates):
        offered = aws.call(
            "ec2",
            "describe-instance-type-offerings",
            "--location-type",
            "availability-zone",
            "--filters",
            f"Name=instance-type,Values={args.relay_instance},{sender}",
        )["InstanceTypeOfferings"]
        zones = {
            row["Location"] for row in offered if row["InstanceType"] == sender
        } & {
            row["Location"]
            for row in offered
            if row["InstanceType"] == args.relay_instance
        }
        if not zones:
            raise RuntimeError(
                f"No shared availability zone for {args.relay_instance} and {sender}"
            )
        summary = execute(
            aws, dict(base, az=sorted(zones)[0]), sender, out / f"attempt-{index+1}"
        )
        print(json.dumps(summary), flush=True)
        if not summary.get("retrySender"):
            return 0 if summary["status"] == "PASS" else 1
        if index + 1 < len(candidates):
            print(
                "Supporting capacity was insufficient; trying the next sender size.",
                flush=True,
            )
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
