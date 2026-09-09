import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

from janitor import cleanup
from run import unpack_evidence, source_bundle
import host


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.ec2, self.s3, self.cfn = Mock(), Mock(), Mock()
        self.clients = (self.ec2, self.s3, self.cfn)
        self.cfg = dict(
            run="relay-perf-0123456789ab",
            bucket="isolated",
            stack="workload",
            guard="guard",
            role="scoped-role",
            expires=100,
        )
        self.s3.list_objects_v2.return_value = {}
        self.ec2.describe_instances.return_value = {"Reservations": []}
        self.cfn.describe_stacks.return_value = {
            "Stacks": [{"StackStatus": "DELETE_COMPLETE"}]
        }

    def test_waits_without_touching_compute(self):
        self.assertEqual(cleanup(self.clients, self.cfg, 1)["status"], "waiting")
        self.ec2.describe_instances.assert_not_called()

    def test_completion_stops_only_owned_compute_before_deleting_stack(self):
        self.s3.list_objects_v2.return_value = {
            "Contents": [{"Key": "results/summary.json"}]
        }
        self.ec2.describe_instances.return_value = {
            "Reservations": [{"Instances": [{"InstanceId": "owned"}]}]
        }
        self.assertEqual(cleanup(self.clients, self.cfg, 1)["status"], "terminating")
        self.ec2.terminate_instances.assert_called_once_with(InstanceIds=["owned"])
        filters = self.ec2.describe_instances.call_args.kwargs["Filters"]
        self.assertEqual(
            filters[0], {"Name": "tag:RelayPerfRun", "Values": [self.cfg["run"]]}
        )
        self.cfn.delete_stack.assert_not_called()

    def test_retry_workload_delete_before_removing_watchdog(self):
        self.cfn.describe_stacks.return_value = {
            "Stacks": [{"StackStatus": "DELETE_FAILED"}]
        }
        self.assertEqual(
            cleanup(self.clients, self.cfg, 101)["status"], "deleting_workload"
        )
        self.cfn.delete_stack.assert_called_once_with(
            StackName="workload", RoleARN="scoped-role"
        )

    def test_retains_downloadable_evidence_until_expiry(self):
        self.s3.list_objects_v2.return_value = {
            "Contents": [{"Key": "results/summary.json"}]
        }
        self.assertEqual(cleanup(self.clients, self.cfg, 1)["status"], "evidence_ready")
        self.s3.delete_objects.assert_not_called()

    def test_expiry_empties_bucket_before_removing_guard(self):
        self.s3.list_objects_v2.side_effect = [
            {},
            {"Contents": [{"Key": "private/key"}]},
            {},
        ]
        self.s3.delete_objects.return_value = {}
        self.assertEqual(cleanup(self.clients, self.cfg, 101)["status"], "deleting")
        self.cfn.delete_stack.assert_called_once_with(
            StackName="guard", RoleARN="scoped-role"
        )

    def test_failed_artifact_cleanup_preserves_watchdog(self):
        self.s3.list_objects_v2.return_value = {"Contents": [{"Key": "private/key"}]}
        self.s3.delete_objects.return_value = {"Errors": [{"Code": "AccessDenied"}]}
        with self.assertRaises(RuntimeError):
            cleanup(self.clients, self.cfg, 101)
        self.cfn.delete_stack.assert_not_called()

    def test_evidence_cannot_escape_output_directory(self):
        with tempfile.TemporaryDirectory() as temp:
            archive = Path(temp) / "evidence.tar.gz"
            for name, kind in [
                ("../escaped", tarfile.REGTYPE),
                ("link", tarfile.SYMTYPE),
            ]:
                with tarfile.open(archive, "w:gz") as tar:
                    entry = tarfile.TarInfo(name)
                    entry.type = kind
                    tar.addfile(entry, io.BytesIO())
                with self.assertRaises(ValueError):
                    unpack_evidence(archive, Path(temp) / "out")

    def test_source_rejects_private_material_and_parent_symlinks(self):
        with (
            tempfile.TemporaryDirectory() as temp,
            tempfile.TemporaryDirectory() as outside,
        ):
            root = Path(temp)
            (root / "tools").mkdir()
            (root / "tools" / "escape").symlink_to(outside, target_is_directory=True)
            (Path(outside) / "file.go").write_text("outside")
            (root / "tools" / "private.key").write_text("private")
            for name in ["tools/escape/file.go", "tools/private.key"]:
                with (
                    patch("run.ROOT", root),
                    patch(
                        "run.subprocess.check_output",
                        return_value=(name + "\0").encode(),
                    ),
                ):
                    with self.assertRaises(ValueError):
                        source_bundle(root / "source.tar.gz")

    def test_killed_harness_still_exports_sender_oom_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = dict(
                sender="t3a.micro",
                relay="t3a.nano",
                revision="a" * 40,
                workload="clinic-mixed-v1",
                cpus=1,
                memory="256MiB",
            )
            (root / "run.json").write_text(json.dumps(cfg))

            def process(args, **_kwargs):
                if args[0] == "ssh":
                    return Mock(returncode=0)
                if args[0] == "sh":
                    return Mock(stdout="Out of memory: Killed process relay-perf\n")
                return Mock(returncode=-9)

            with (
                patch.object(host, "BASE", root),
                patch.object(host, "run"),
                patch.object(host.subprocess, "run", side_effect=process),
                patch.object(host.socket, "gethostbyname", return_value="10.83.0.10"),
                patch(
                    "sys.argv",
                    ["host.py", "--relay", "10.83.0.11", "--bucket", "owned"],
                ),
                patch.dict(os.environ),
            ):
                host.main()
            result = json.loads((root / "results/summary.json").read_text())
            self.assertTrue(result["retrySender"])
            self.assertTrue(result["senderOOM"])
            self.assertNotEqual(result["status"], "PASS")
            self.assertTrue((root / "results.tar.gz").is_file())


if __name__ == "__main__":
    unittest.main()
