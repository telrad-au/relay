"""Per-run watchdog: bounded compute lifetime and deletion of this stack only."""

import json
import os
import time


def cleanup(clients, config, now, requested=False):
    ec2, s3, cfn = clients
    completed = bool(
        s3.list_objects_v2(
            Bucket=config["bucket"], Prefix="results/summary.json", MaxKeys=1
        ).get("Contents")
    )
    remove_evidence = requested or now >= config["expires"]
    if not remove_evidence and not completed:
        return {"status": "waiting", "expires": config["expires"]}
    # Stop spending on compute first, including a failed/incomplete deployment.
    reservations = ec2.describe_instances(
        Filters=[
            {"Name": "tag:RelayPerfRun", "Values": [config["run"]]},
            {
                "Name": "instance-state-name",
                "Values": ["pending", "running", "stopping", "stopped"],
            },
        ]
    )["Reservations"]
    ids = [i["InstanceId"] for r in reservations for i in r["Instances"]]
    if ids:
        ec2.terminate_instances(InstanceIds=ids)
        return {"status": "terminating", "count": len(ids)}
    try:
        stacks = cfn.describe_stacks(StackName=config["stack"])["Stacks"]
        state = stacks[0]["StackStatus"]
    except Exception as exc:
        # A missing exact stack is the expected state before creation or after
        # successful deletion. Authorization/transport errors are never hidden.
        error = getattr(exc, "response", {}).get("Error", {})
        if error.get("Code") != "ValidationError" or "does not exist" not in error.get(
            "Message", ""
        ):
            raise
        state = "DELETE_COMPLETE"
    if state != "DELETE_COMPLETE":
        if state != "DELETE_IN_PROGRESS":
            cfn.delete_stack(StackName=config["stack"], RoleARN=config["role"])
        return {"status": "deleting_workload"}
    if not remove_evidence:
        return {"status": "evidence_ready", "expires": config["expires"]}
    # No workload can write more objects once both VMs have terminated.
    while True:
        page = s3.list_objects_v2(Bucket=config["bucket"])
        objects = [{"Key": obj["Key"]} for obj in page.get("Contents", [])]
        if not objects:
            break
        response = s3.delete_objects(
            Bucket=config["bucket"], Delete={"Objects": objects, "Quiet": True}
        )
        if response.get("Errors"):
            raise RuntimeError("artifact_cleanup_failed")
    cfn.delete_stack(StackName=config["guard"], RoleARN=config["role"])
    return {"status": "deleting"}


def handler(event, context):
    import boto3

    config = json.loads(os.environ["RUN_CONFIG"])
    # Only this function's IAM-authorized caller can request early cleanup.
    return cleanup(
        tuple(boto3.client(name) for name in ("ec2", "s3", "cloudformation")),
        config,
        int(time.time()),
        event.get("cleanup") is True,
    )
