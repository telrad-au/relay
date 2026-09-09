import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import * as cdk from "aws-cdk-lib";
import { Template } from "aws-cdk-lib/assertions";
import { makeStacks } from "./stack.mjs";

const config = () => ({
  run: "relay-perf-0123456789ab",
  account: "123456789012",
  region: "ap-southeast-2",
  az: "ap-southeast-2a",
  ami: "ami-0123456789abcdef0",
  relay: "t3a.nano",
  sender: "t3a.micro",
  expires: Math.floor(Date.now() / 1000) + 3600,
});
test("isolated network, bounded compute and independent cleanup survive client loss", () => {
  const stacks = makeStacks(new cdk.App(), config());
  const guard = Template.fromStack(stacks.guard).toJSON();
  const workload = Template.fromStack(stacks.workload).toJSON();
  const resources = Object.values(workload.Resources);
  const instances = resources.filter((r) => r.Type === "AWS::EC2::Instance");
  assert.equal(instances.length, 2);
  for (const instance of instances) {
    assert.equal(instance.Properties.MetadataOptions.HttpTokens, "required");
    assert.equal(
      instance.Properties.CreditSpecification.CPUCredits,
      "unlimited",
    );
    assert.equal(
      instance.Properties.BlockDeviceMappings[0].Ebs.Encrypted,
      true,
    );
    assert.equal(
      instance.Properties.BlockDeviceMappings[0].Ebs.DeleteOnTermination,
      true,
    );
  }
  const relay = instances.find((i) => i.Properties.InstanceType === "t3a.nano");
  const bootstrap = JSON.stringify(relay.Properties.UserData);
  assert.ok(bootstrap.includes("swapon /var/tmp/relay-perf-bootstrap.swap"));
  assert.ok(bootstrap.includes("swapoff -a"));
  assert.ok(bootstrap.includes("trap 'swapoff"));
  for (const rule of resources.filter(
    (r) => r.Type === "AWS::EC2::SecurityGroupIngress",
  )) {
    assert.ok(rule.Properties.SourceSecurityGroupId);
    assert.equal(rule.Properties.CidrIp, undefined);
  }
  assert.equal(resources.filter((r) => r.Type === "AWS::EC2::VPC").length, 1);
  assert.equal(
    resources.filter((r) =>
      [
        "AWS::EC2::EIP",
        "AWS::EC2::NatGateway",
        "AWS::EC2::VPCPeeringConnection",
      ].includes(r.Type),
    ).length,
    0,
  );
  const guardResources = Object.values(guard.Resources);
  for (const resource of [...guardResources, ...resources]) {
    assert.notEqual(resource.DeletionPolicy, "Retain");
    assert.notEqual(resource.UpdateReplacePolicy, "Retain");
  }
  for (const role of guardResources.filter(
    (r) =>
      r.Type === "AWS::IAM::Role" &&
      !r.Properties.RoleName.endsWith("-cleanup"),
  )) {
    assert.equal(role.Properties.ManagedPolicyArns, undefined);
    assert.ok(!JSON.stringify(role).includes("ssm:GetParameter"));
    assert.ok(!JSON.stringify(role).includes("s3:ListBucket"));
  }
  assert.ok(
    guardResources.some(
      (r) =>
        r.Type === "AWS::Events::Rule" &&
        r.Properties.ScheduleExpression === "rate(1 minute)",
    ),
  );
  const role = guardResources.find(
    (r) =>
      r.Type === "AWS::IAM::Role" && r.Properties.RoleName.endsWith("-cleanup"),
  );
  for (const statement of role.Properties.Policies[0].PolicyDocument
    .Statement) {
    const actions = [statement.Action].flat();
    if (actions.some((a) => a.startsWith("ec2:") && !a.includes("Describe"))) {
      assert.equal(
        statement.Condition.StringEquals["ec2:ResourceTag/RelayPerfRun"],
        config().run,
      );
    }
  }
  assert.ok(!JSON.stringify(guard).includes("CDKToolkit"));
  assert.equal(workload.Parameters?.BootstrapVersion, undefined);
  assert.ok(!JSON.stringify(workload).includes("/cdk-bootstrap/"));
});
test("Docker build contexts exclude CDK dependencies and generated output", () => {
  const ignored = fs
    .readFileSync(new URL("../../../.dockerignore", import.meta.url), "utf8")
    .split("\n");
  for (const path of ["**/node_modules", "**/cdk.out", "**/__pycache__"])
    assert.ok(ignored.includes(path));
});
test("rejects unbounded identities and instance choices", () => {
  for (const patch of [
    { run: "production" },
    { sender: "c6a.48xlarge" },
    { expires: Math.floor(Date.now() / 1000) + 86400 },
  ]) {
    assert.throws(() => makeStacks(new cdk.App(), { ...config(), ...patch }));
  }
});
