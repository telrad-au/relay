import fs from "node:fs";
import { fileURLToPath } from "node:url";
import * as cdk from "aws-cdk-lib";

const here = fileURLToPath(new URL(".", import.meta.url));
const {
  aws_ec2: ec2,
  aws_iam: iam,
  aws_s3: s3,
  aws_lambda: lambda,
  aws_events: events,
  aws_logs: logs,
} = cdk;
export const senderCandidates = [
  "t3a.micro",
  "t3a.small",
  "c6a.large",
  "c6a.xlarge",
];
export function makeStacks(app, cfg) {
  if (
    !/^relay-perf-[0-9a-f]{12}$/.test(cfg.run) ||
    !/^\d{12}$/.test(cfg.account) ||
    !/^[a-z]{2}-[a-z]+-\d$/.test(cfg.region)
  )
    throw Error("Invalid isolated run identity");
  if (
    !senderCandidates.includes(cfg.sender) ||
    !["t3a.nano", "t3a.micro", "t3a.small"].includes(cfg.relay)
  )
    throw Error("Instance type outside the bounded small-instance ladder");
  if (
    !Number.isSafeInteger(cfg.expires) ||
    cfg.expires <= Math.floor(Date.now() / 1000) ||
    cfg.expires > Date.now() / 1000 + 10800
  )
    throw Error("Expiry must be in the next three hours");
  const stack = new cdk.Stack(app, cfg.run, {
    env: { account: cfg.account, region: cfg.region },
    synthesizer: new cdk.CliCredentialsStackSynthesizer(),
  });
  const workload = new cdk.Stack(app, `${cfg.run}-workload`, {
    env: { account: cfg.account, region: cfg.region },
    synthesizer: new cdk.CliCredentialsStackSynthesizer(),
  });
  workload.addStackDependency(stack);
  const name = cfg.run,
    bucketName = `${name}-${cfg.account}-${cfg.region}`;
  const arn = (service, resource) =>
    `arn:aws:${service}:${cfg.region}:${cfg.account}:${resource}`;
  const iamArn = (resource) => `arn:aws:iam::${cfg.account}:${resource}`;
  const tag = [
    { key: "RelayPerfRun", value: name },
    { key: "Purpose", value: "synthetic-relay-performance" },
  ];
  const policy = (Statement) => ({ Version: "2012-10-17", Statement });
  const allow = (Action, Resource, Condition) => ({
    Effect: "Allow",
    Action,
    Resource,
    ...(Condition ? { Condition } : {}),
  });
  const trust = (services) =>
    policy([
      {
        Effect: "Allow",
        Principal: { Service: services },
        Action: "sts:AssumeRole",
      },
    ]);
  const cleanupRole = new iam.CfnRole(stack, "CleanupRole", {
    roleName: `${name}-cleanup`,
    assumeRolePolicyDocument: trust([
      "lambda.amazonaws.com",
      "cloudformation.amazonaws.com",
    ]),
    policies: [
      {
        policyName: "OnlyThisRun",
        policyDocument: policy([
          allow(["ec2:Describe*"], "*"),
          allow(
            [
              "ec2:TerminateInstances",
              "ec2:DeleteVpc",
              "ec2:DeleteSubnet",
              "ec2:DeleteSecurityGroup",
              "ec2:DeleteRouteTable",
              "ec2:DeleteRoute",
              "ec2:DeleteInternetGateway",
              "ec2:DetachInternetGateway",
              "ec2:DisassociateRouteTable",
              "ec2:RevokeSecurityGroupIngress",
              "ec2:RevokeSecurityGroupEgress",
            ],
            `arn:aws:ec2:${cfg.region}:${cfg.account}:*`,
            { StringEquals: { "ec2:ResourceTag/RelayPerfRun": name } },
          ),
          allow(
            [
              "s3:ListBucket",
              "s3:DeleteBucket",
              "s3:DeleteBucketPolicy",
              "s3:GetBucketLocation",
              "s3:GetBucketTagging",
            ],
            `arn:aws:s3:::${bucketName}`,
          ),
          allow(["s3:DeleteObject"], `arn:aws:s3:::${bucketName}/*`),
          allow(
            [
              "cloudformation:DeleteStack",
              "cloudformation:DescribeStacks",
              "cloudformation:DescribeStackResources",
              "cloudformation:ListStackResources",
            ],
            [
              arn("cloudformation", `stack/${name}/*`),
              arn("cloudformation", `stack/${name}-workload/*`),
            ],
          ),
          allow(["iam:PassRole"], iamArn(`role/${name}-cleanup`), {
            StringEquals: {
              "iam:PassedToService": "cloudformation.amazonaws.com",
            },
          }),
          allow(
            [
              "iam:GetRole",
              "iam:GetRolePolicy",
              "iam:DeleteRole",
              "iam:DeleteRolePolicy",
              "iam:ListRolePolicies",
              "iam:ListAttachedRolePolicies",
              "iam:DetachRolePolicy",
            ],
            iamArn(`role/${name}-*`),
          ),
          allow(
            [
              "iam:GetInstanceProfile",
              "iam:RemoveRoleFromInstanceProfile",
              "iam:DeleteInstanceProfile",
            ],
            iamArn(`instance-profile/${name}-*`),
          ),
          allow(
            [
              "lambda:GetFunction",
              "lambda:GetPolicy",
              "lambda:DeleteFunction",
              "lambda:RemovePermission",
            ],
            arn("lambda", `function:${name}-cleanup`),
          ),
          allow(
            [
              "events:DescribeRule",
              "events:ListTargetsByRule",
              "events:RemoveTargets",
              "events:DeleteRule",
            ],
            arn("events", `rule/${name}-expiry`),
          ),
          allow(["logs:DescribeLogGroups"], "*"),
          allow(
            [
              "logs:CreateLogStream",
              "logs:PutLogEvents",
              "logs:DeleteLogGroup",
            ],
            arn("logs", `log-group:/aws/lambda/${name}-cleanup:*`),
          ),
        ]),
      },
    ],
    tags: tag,
  });
  const bucket = new s3.CfnBucket(stack, "Artifacts", {
    bucketName,
    publicAccessBlockConfiguration: {
      blockPublicAcls: true,
      blockPublicPolicy: true,
      ignorePublicAcls: true,
      restrictPublicBuckets: true,
    },
    bucketEncryption: {
      serverSideEncryptionConfiguration: [
        { serverSideEncryptionByDefault: { sseAlgorithm: "AES256" } },
      ],
    },
    lifecycleConfiguration: {
      rules: [
        {
          id: "AbortUploads",
          status: "Enabled",
          abortIncompleteMultipartUpload: { daysAfterInitiation: 1 },
        },
      ],
    },
    tags: tag,
  });
  new s3.CfnBucketPolicy(stack, "TLSOnly", {
    bucket: bucket.ref,
    policyDocument: policy([
      {
        Effect: "Deny",
        Principal: "*",
        Action: "s3:*",
        Resource: [
          `arn:aws:s3:::${bucketName}`,
          `arn:aws:s3:::${bucketName}/*`,
        ],
        Condition: { Bool: { "aws:SecureTransport": "false" } },
      },
    ]),
  });
  const log = new logs.CfnLogGroup(stack, "CleanupLog", {
    logGroupName: `/aws/lambda/${name}-cleanup`,
    retentionInDays: 1,
  });
  const janitor = new lambda.CfnFunction(stack, "Janitor", {
    functionName: `${name}-cleanup`,
    role: cleanupRole.attrArn,
    runtime: "python3.13",
    handler: "index.handler",
    timeout: 60,
    memorySize: 128,
    code: { zipFile: fs.readFileSync(`${here}janitor.py`, "utf8") },
    environment: {
      variables: {
        RUN_CONFIG: stack.toJsonString({
          run: name,
          bucket: bucketName,
          stack: `${name}-workload`,
          guard: name,
          role: cleanupRole.attrArn,
          expires: cfg.expires,
        }),
      },
    },
    tags: tag,
  });
  janitor.addResourceDependency(log);
  janitor.addResourceDependency(bucket);
  const rule = new events.CfnRule(stack, "Expiry", {
    name: `${name}-expiry`,
    scheduleExpression: "rate(1 minute)",
    state: "ENABLED",
    targets: [{ id: "Cleanup", arn: janitor.attrArn }],
  });
  const invoke = new lambda.CfnPermission(stack, "AllowExpiry", {
    action: "lambda:InvokeFunction",
    functionName: janitor.ref,
    principal: "events.amazonaws.com",
    sourceArn: rule.attrArn,
    sourceAccount: cfg.account,
  });
  const vpc = new ec2.CfnVPC(workload, "VPC", {
    cidrBlock: "10.83.0.0/24",
    enableDnsHostnames: true,
    enableDnsSupport: true,
    tags: tag,
  });
  const subnet = new ec2.CfnSubnet(workload, "Subnet", {
    vpcId: vpc.ref,
    cidrBlock: "10.83.0.0/24",
    availabilityZone: cfg.az,
    mapPublicIpOnLaunch: true,
    tags: tag,
  });
  const igw = new ec2.CfnInternetGateway(workload, "InternetGateway", {
    tags: tag,
  });
  const attachment = new ec2.CfnVPCGatewayAttachment(
    workload,
    "GatewayAttachment",
    { vpcId: vpc.ref, internetGatewayId: igw.ref },
  );
  const routes = new ec2.CfnRouteTable(workload, "RouteTable", {
    vpcId: vpc.ref,
    tags: tag,
  });
  const association = new ec2.CfnSubnetRouteTableAssociation(
    workload,
    "Routes",
    { routeTableId: routes.ref, subnetId: subnet.ref },
  );
  const route = new ec2.CfnRoute(workload, "InternetRoute", {
    routeTableId: routes.ref,
    destinationCidrBlock: "0.0.0.0/0",
    gatewayId: igw.ref,
  });
  route.addResourceDependency(attachment);
  const relaySG = new ec2.CfnSecurityGroup(workload, "RelaySecurityGroup", {
    vpcId: vpc.ref,
    groupDescription: "Only the isolated sender reaches Relay",
    tags: tag,
  });
  const senderSG = new ec2.CfnSecurityGroup(workload, "SenderSecurityGroup", {
    vpcId: vpc.ref,
    groupDescription: "Only Relay reaches simulated cloud and RIS",
    tags: tag,
  });
  for (const port of [22, 11112, 2575])
    new ec2.CfnSecurityGroupIngress(workload, `RelayPort${port}`, {
      groupId: relaySG.attrGroupId,
      sourceSecurityGroupId: senderSG.attrGroupId,
      ipProtocol: "tcp",
      fromPort: port,
      toPort: port,
    });
  for (const port of [8443, 2576])
    new ec2.CfnSecurityGroupIngress(workload, `SupportPort${port}`, {
      groupId: senderSG.attrGroupId,
      sourceSecurityGroupId: relaySG.attrGroupId,
      ipProtocol: "tcp",
      fromPort: port,
      toPort: port,
    });
  const profile = (role) => {
    const reads =
      role === "sender"
        ? [
            "source.tar.gz",
            "run.json",
            "private/sender_key",
            "private/relay_host_key.pub",
          ]
        : ["private/relay_host_key", "private/sender_key.pub"];
    const statements = [
      allow(
        "s3:GetObject",
        reads.map((key) => `arn:aws:s3:::${bucketName}/${key}`),
      ),
    ];
    statements.push(
      allow(
        [
          "ssm:UpdateInstanceInformation",
          "ssmmessages:CreateControlChannel",
          "ssmmessages:CreateDataChannel",
          "ssmmessages:OpenControlChannel",
          "ssmmessages:OpenDataChannel",
          "ec2messages:AcknowledgeMessage",
          "ec2messages:DeleteMessage",
          "ec2messages:FailMessage",
          "ec2messages:GetEndpoint",
          "ec2messages:GetMessages",
          "ec2messages:SendReply",
        ],
        "*",
      ),
    );
    if (role === "sender")
      statements.push(
        allow("s3:PutObject", `arn:aws:s3:::${bucketName}/results/*`),
      );
    const identity = new iam.CfnRole(stack, `${role}Role`, {
      roleName: `${name}-${role}`,
      assumeRolePolicyDocument: trust(["ec2.amazonaws.com"]),
      policies: [
        { policyName: "RunArtifacts", policyDocument: policy(statements) },
      ],
      tags: tag,
    });
    return new iam.CfnInstanceProfile(stack, `${role}Profile`, {
      instanceProfileName: `${name}-${role}`,
      roles: [identity.ref],
    });
  };
  const base = (role) =>
    `#!/bin/bash\nset -eu\nexport AWS_DEFAULT_REGION='${cfg.region}'\n${role === "relay" ? "fallocate -l 1G /var/tmp/relay-perf-bootstrap.swap\nchmod 600 /var/tmp/relay-perf-bootstrap.swap\nmkswap /var/tmp/relay-perf-bootstrap.swap >/dev/null\nswapon /var/tmp/relay-perf-bootstrap.swap\ntrap 'swapoff -a; rm -f /var/tmp/relay-perf-bootstrap.swap' EXIT\n" : ""}dnf install -y docker git python3 openssh-clients\nsystemctl enable --now docker\nusermod -aG docker ec2-user\ninstall -d -m 700 /opt/relay-perf\nfetch() { for attempt in $(seq 1 120); do aws s3 cp --only-show-errors "s3://${bucketName}/$1" "$2" 2>/dev/null && return; sleep 5; done; return 1; }\n`;
  const relayData =
    base("relay") +
    `fetch private/relay_host_key /etc/ssh/ssh_host_ed25519_key\nchmod 600 /etc/ssh/ssh_host_ed25519_key\nssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key > /etc/ssh/ssh_host_ed25519_key.pub\nprintf 'HostKey /etc/ssh/ssh_host_ed25519_key\\n' > /etc/ssh/sshd_config.d/00-perf-hostkey.conf\ninstall -d -m 700 -o ec2-user -g ec2-user /home/ec2-user/.ssh\nfetch private/sender_key.pub /home/ec2-user/.ssh/authorized_keys\nchown ec2-user:ec2-user /home/ec2-user/.ssh/authorized_keys\nchmod 600 /home/ec2-user/.ssh/authorized_keys\nsystemctl restart sshd\ntouch /opt/relay-perf/ready\n`;
  const instance = (role, type, sg, userData) => {
    const value = new ec2.CfnInstance(
      workload,
      role === "relay" ? "Relay" : "Sender",
      {
        imageId: cfg.ami,
        instanceType: type,
        subnetId: subnet.ref,
        securityGroupIds: [sg.attrGroupId],
        iamInstanceProfile: profile(role).ref,
        userData: cdk.Fn.base64(userData),
        metadataOptions: { httpTokens: "required", httpPutResponseHopLimit: 1 },
        creditSpecification: type.startsWith("t3")
          ? { cpuCredits: "unlimited" }
          : undefined,
        blockDeviceMappings: [
          {
            deviceName: "/dev/xvda",
            ebs: {
              volumeSize: role === "relay" ? 8 : 24,
              volumeType: "gp3",
              encrypted: true,
              deleteOnTermination: true,
            },
          },
        ],
        tags: [...tag, { key: "Name", value: `${name}-${role}` }],
        propagateTagsToVolumeOnCreation: true,
      },
    );
    for (const dependency of [route, association])
      value.addResourceDependency(dependency);
    return value;
  };
  const relay = instance("relay", cfg.relay, relaySG, relayData);
  const senderData = cdk.Fn.join("", [
    base("sender"),
    `fetch source.tar.gz /opt/relay-perf/source.tar.gz\nfetch run.json /opt/relay-perf/run.json\nfetch private/sender_key /root/.ssh-perf-key\nchmod 600 /root/.ssh-perf-key\ninstall -d -m 700 /root/.ssh\nfetch private/relay_host_key.pub /root/.ssh-perf-host.pub\nprintf '%s %s\\n' '`,
    relay.attrPrivateIp,
    `' "$(cat /root/.ssh-perf-host.pub)" > /root/.ssh/known_hosts\nprintf 'Host *\\n  IdentityFile /root/.ssh-perf-key\\n  IdentitiesOnly yes\\n  StrictHostKeyChecking yes\\n  BatchMode yes\\n' > /root/.ssh/config\nmkdir /opt/relay-perf/source\ntar -xzf /opt/relay-perf/source.tar.gz -C /opt/relay-perf/source\ncd /opt/relay-perf/source\npython3 tools/relay-perf/aws/host.py --relay '`,
    relay.attrPrivateIp,
    `' --bucket '${bucketName}' > /opt/relay-perf/controller.log 2>&1\n`,
  ]);
  const sender = instance("sender", cfg.sender, senderSG, senderData);
  // Cleanup role is deleted last, after every resource it authorizes deleting.
  for (const resource of stack.node.findAll())
    if (resource instanceof cdk.CfnResource && resource !== cleanupRole)
      resource.addResourceDependency(cleanupRole);
  for (const [key, value] of Object.entries({
    Bucket: bucketName,
    Janitor: janitor.ref,
  }))
    new cdk.CfnOutput(stack, key + "Output", { value }).overrideLogicalId(key);
  for (const [key, value] of Object.entries({
    Relay: relay.ref,
    Sender: sender.ref,
    RelayIP: relay.attrPrivateIp,
    SenderIP: sender.attrPrivateIp,
  }))
    new cdk.CfnOutput(workload, key + "Output", { value }).overrideLogicalId(
      key,
    );
  return { guard: stack, workload };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const cfg = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
  const app = new cdk.App({ outdir: process.argv[3] });
  const stacks = makeStacks(app, cfg);
  app.synth();
  console.log(
    JSON.stringify(
      Object.fromEntries(
        Object.entries(stacks).map(([key, stack]) => [
          key,
          `${app.outdir}/${stack.stackName}.template.json`,
        ]),
      ),
    ),
  );
}
