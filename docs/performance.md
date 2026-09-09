# Performance qualification

Issue [#4](https://github.com/telrad-au/relay/issues/4) establishes repeatable
measurements before publishing a minimum tested configuration. The harness,
benchmarks, and observations below use synthetic data. No minimum machine
specification has been qualified. Linux x64 and Windows x64 qualification needs
separately supplied native machines, and upload recovery remains blocked by
[#16](https://github.com/telrad-au/relay/issues/16). ARM requirements remain
unpublished. Optimizations belong in #5/#6.

## Reproduce a run

Use the repository root, Go **1.27.0**, and a Docker daemon running Linux
containers with cgroup v2, CPU quota, memory/swap accounting, and `netem` support.
Allow image downloads and local test sockets. Run one performance experiment at
a time on an otherwise idle supporting host. Docker Desktop is useful for
correctness and screening; it cannot establish native Windows requirements.

```bash
go run ./tools/relay-perf screen --cpus 1 --memory 256MiB --out dist/perf-mixed
go run ./tools/relay-perf smoke --out dist/perf-smoke
go run ./tools/relay-perf sweep --out dist/perf-screen
go run ./tools/relay-perf fault-check --out dist/perf-faults
# Continue only after fault recovery passes.
# Use the smallest passing screening point; these example values are exploratory.
go run ./tools/relay-perf qualify \
  --profile clinic-v1 --cpus 1 --memory 256MiB --out dist/perf
```

Every output directory must be new. SIGINT/SIGTERM cancels work, exports available
evidence, and removes only this run's containers, network, volume, temporary
credentials/CA, fixture volume, and image tags. Cleanup failures fail the run and remain in the
result. A killed controller or inaccessible Docker daemon can prevent cleanup;
inspect the recorded container names and `telrad.perf` labels before removing
those resources. Never use a host-wide prune as cleanup.

| Command | Measurement interval | Meaning of PASS |
| --- | --- | --- |
| `screen` | Uses the selected profile; mixed default: 10 s idle, 480 s nominal, 96 s headroom, 180 s recovery | One resource point; complete mixed studies, declared size deadlines, integrity and memory headroom |
| `smoke` | 5 s idle, 45 s nominal, 15 s headroom, 15 s recovery | Correctness and enforced resource limits; no latency, throughput, or 80% memory timing gate |
| `sweep` | 10 s idle, 240 s nominal, 60 s headroom, 30 s recovery per point | Screening only; checks traffic latency and memory headroom |
| `experiment --case NAME` | Same as screening | Only the explicitly recorded experimental workload |
| `fault-check` | Smoke-length traffic, followed by real fault/retry timers | Fault recovery plus traffic/resource criteria; currently fails on #16 |
| `qualify` | Three full one-hour runs plus calibration and fault checks | Container workload qualification; native/lifecycle evidence is still required for a published minimum |
| `external` | Three full one-hour native runs | Exports workload evidence; remains INCONCLUSIVE pending independent native resource/lifecycle review |
| `external-fault-check` | Short traffic run plus faults against a native service | Uses separately declared 5 s DICOM idle / 15 s lifetime configuration |

`--duration 5s` shortens the nominal interval for a smoke, screening, or fault
check; headroom becomes one quarter of that duration (at least one second).
For `screen`, headroom is one fifth of the requested nominal duration and the
profile's recovery allowance is preserved. Very short mixed runs do not exercise
the full modality distribution. `screen` is a screening result, not qualification.
Qualification and `external` reject duration overrides. CI runs a bounded smoke
and single-iteration benchmark checks, with no throughput timing threshold.
Optional `RELAY_PERF_COVERAGE_DIR` instruments **only supporting test helpers** in
smoke mode; such runs are correctness evidence. Relay remains uninstrumented.

### Hardware screen latency accounting

Following the first AWS mixed-study run, `screen` now applies latency limits
from `started` to `finished`, excluding waiting for a sender worker. For a study,
this is first instance start through last instance completion. It still includes
transfer, cloud receipt and protocol acknowledgement time, plus any waiting
between that study's instances. This measures an active transfer, not Relay CPU
processing alone.

Each summary exports `senderQueue` and `transfer` p50/p95/p99/max distributions.
The existing top-level latency percentiles remain scheduled-to-finished for
historical comparison. `latencyAcceptanceBasis` explicitly identifies which
clock determines the verdict. Sender queue delay alone no longer fails `screen`;
all offered work must still be accounted for and completed by recovery's end.
Integrity, resource, calibration and fault assertions are unchanged. Other
commands, including full qualification, retain end-to-end latency acceptance.
Earlier result files keep their original verdict and acceptance basis.

Screen in this order: 2 CPUs/512 MiB, then 1 CPU with 512/256/128/64 MiB. `sweep`
stops at the first FAIL or INCONCLUSIVE point. A screening pass does not establish
an hour-long memory plateau or fault recovery. Run full qualification at the
smallest passing point, and retain the preceding points' evidence.

## Disposable AWS screening

Run the mixed study screen without building images or generating traffic on the
local computer:

```bash
python3 tools/relay-perf/aws/run.py
# Optional named CLI profile and region:
python3 tools/relay-perf/aws/run.py --aws-profile test --region ap-southeast-2 \
  --out dist/perf-aws
```

The local entry point needs Python 3.11+, Node/npm, Git, OpenSSH and AWS CLI v2.
It uses the normal AWS CLI credential chain, including the current `aws login`
session. Credentials are never copied into an instance, image, archive or result.
The caller needs permission to create and delete the run's CloudFormation, IAM,
EC2/VPC, S3, Lambda, EventBridge and CloudWatch resources. CDK is pinned in a
private test package. Direct deployment of its synthesized templates needs no
account-wide CDK bootstrap stack.

Each attempt creates a fresh VPC, subnet, security groups, encrypted temporary
S3 bucket, dedicated roles and two Linux x64 EC2 VMs in one availability zone.
Test and SSH ports accept traffic only from the other VM's security group.
Ephemeral public addresses provide outbound downloads and Systems Manager access.
There is no public ingress, NAT gateway, Elastic IP or peering. Existing assets
are not imported. SSH uses per-attempt keys and a pinned server host key; Docker
is reached over SSH only.

Relay starts on `t3a.nano` (512 MiB machine RAM), with its release container
limited to 1 CPU and 256 MiB, memory-plus-swap equal to memory. Its encrypted
8 GiB root disk is deleted on termination. The sender starts on `c6a.large`
(2 vCPU, 4 GiB), the smallest tested candidate to pass supporting calibration,
then tries `c6a.xlarge` only when supporting calibration or a setup OOM
demonstrates insufficient capacity. The smaller `t3a.micro` and `t3a.small`
candidates remain available explicitly to reproduce the observations below.
Each attempt is removed
before another starts. These are bounded candidates, not a comparison of every
EC2 family. `--sender-instance` selects one explicitly; `--relay-instance`,
`--cpus`, and `--memory` change the target independently. The sender has a
24 GiB encrypted disposable disk.

T3a uses Unlimited credit mode. EC2, surplus CPU credits, public IPv4 addresses,
storage and supporting AWS services incur charges. Results include configured
credit modes and CloudWatch observations; absent data points do not prove zero
charges. See [AWS burstable credits](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/burstable-performance-instances-unlimited-mode.html).

Builds and fixture staging use `/var/tmp` on the encrypted disk; Amazon Linux
[uses RAM for `/tmp`](https://docs.aws.amazon.com/linux/al2023/ug/filesystem-slash-tmp.html).
Validation tools use serial compilation and a soft Go heap limit. Measured
helpers and the release executable do not inherit that diagnostic limit.
The small Relay VM uses a temporary 1 GiB swap file on its encrypted disk only
while installing host packages. Bootstrap removes it and disables other host
swap before the sender's readiness check can pass. Neither host swap nor
container swap is enabled during Relay measurement. This addresses package
installer memory demand, not a change to Relay's measured RAM allowance.

The sender performs correctness checks and release builds, validates frozen
synthetic fixtures, calibrates at twice headroom, then runs the same `screen`
workload. Only Relay receives CPU/memory constraints. Sender-wide CPU and available
RAM are observed alongside container CPU, memory, network and disk I/O. Saturation
or missing observations prevents a passing result. The additional
`screen --relay-docker` interface requires separate private IPv4 hosts and the
`ec2-user` SSH account.

Cleanup is independent of the local process. A guard stack is deployed before the
workload stack. Its minute-by-minute AWS watchdog terminates run-tagged VMs and
deletes the workload stack after final evidence is uploaded, retaining the
bucket for download. The local runner downloads results and requests complete
cleanup in `finally`, including on cancellation or failure. If the client
disappears, the watchdog also empties the bucket and deletes its own guard at
expiry (90 minutes by default; `--ttl-minutes` accepts 30–180). Deletion is
asynchronous and retried; expiry is not a CloudFormation completion-time
guarantee. The cleanup role can mutate only this attempt's named or tagged
resources. No host-wide pruning or account-wide deletion is used.

Output contains source SHA-256, synthesized templates, resolved AMI/AZ, instance
inventory, phase/validation logs, performance evidence, credit observations and
`cleanup.json`. Source bundles exclude private keys, credentials and generated
dependency directories. Extraction rejects links, traversal and oversized
archives. The archive includes the current working tree; its revision alone
does not identify uncommitted changes.

An AWS container screening pass applies to the recorded workload/resource point.
It does not establish native Windows requirements, a one-hour memory plateau,
fault recovery or installed lifecycle/update qualification.

### AWS screening observations (9 September 2026)

Runs used Sydney (`ap-southeast-2a`), Amazon Linux 2023 x64, and a `t3a.nano`
Relay target with a 1 CPU / 256 MiB container allowance. Sender selection was
measured separately before accepting any Relay capacity result:

| Sender | Observation | Conclusion |
| --- | --- | --- |
| `t3a.micro` (1 GiB) | Validation passed after moving temporary build/fixture files to disk. The host then became unresponsive during preparation, before Relay was observed running. | Inconclusive; no Relay performance result and no proven cause for the unresponsiveness. |
| `t3a.small` (2 GiB) | Completed fixture preparation and all 5,002 calibration operations. One supporting-host CPU sample reached 90.64%, exceeding the declared 90% saturation threshold. | Inconclusive; sender calibration prevented the Relay traffic phase. |
| `c6a.large` (4 GiB) | Passed supporting calibration; peak supporting-host CPU was 80.30%. Relay then rejected the test server certificate because remote extraction had removed the public CA's read permission for its container user. | Sender capacity demonstrated; this attempt's Relay screen was inconclusive. Extraction now preserves the intended modes, with a regression test. |

The small-sender evidence is in
`dist/perf-aws-small/attempt-1/results/performance/point-1-run-1/result.json`,
run `relay-perf-8a7d9e1bbf3c`, source archive SHA-256
`2038769ad7ae7ab5008c1fa6697c79211b4647f7be7d8e32e959dbabeec9614d`.
Its checks passed: Go race tests, vet, publication/licence audits, Orthanc
C-STORE interoperability, HAPI HL7 fixture validation, benchmark smoke and
govulncheck. No sender OOM was recorded for that attempt. These observations
do not establish Relay's minimum requirements.
The first `c6a.large` attempt is recorded under `dist/perf-aws-c6a/attempt-1`,
run `relay-perf-fca0bd999e0b`. Both attempts' `cleanup.json` files confirm no
remaining active instances, volumes, VPCs or stacks.

#### Completed mixed-study screen

The corrected run, `relay-perf-2863dd94a832`, completed on 9 September 2026
at approximately 06:25 UTC. It used `t3a.nano` for Relay and `c6a.large` for
supporting services. The measured Relay VM reported AMD EPYC 7571, two logical
CPUs and 418.4 MiB usable guest RAM; the EC2 instance allocation is 512 MiB.
Its kernel was `6.18.44-99.149.amzn2023.x86_64`, using AMI
`ami-0453a43f384c588e1`. The release container had an effective 1 CPU quota,
256 MiB hard memory limit and zero permitted swap.

**Result: FAIL on DICOM queue and size-dependent latency limits.** All offered
work nevertheless completed correctly, with no unexpected operation failures,
payload/receipt errors, OOM kills or outstanding work:

| Measurement | Result |
| --- | --- |
| Complete studies | 20: 8 US, 7 DX, 4 CT, 1 MR |
| DICOM instances / encoded bytes | 4,962 / 3,762,979,696 bytes (3.50 GiB) |
| Offered study rate | 120/hour nominal, 150/hour headroom; four associations |
| DICOM instance rate across the 576 s arrival interval | 8.61/s; 6.23 MiB/s |
| HL7 ingest / confirmed returned reports | 20 / 20 |
| HL7 ACK p99 / confirmed report p99 | 100 ms / 3.16 s |
| DICOM arrival-to-completion p99 / maximum | 111.39 s / 113.69 s, including queue wait |
| Study completion maximum | 113.69 s |
| Peak Relay cgroup memory / observed RSS | 22.14 MiB / 17.37 MiB |
| Relay CPU averaged across sampled traffic/recovery | 8.71% of one CPU; no recorded quota throttling |
| Peak sender host CPU, calibration / measured workload | 85.76% / 35.57%; no saturation or missing metrics |
| Sender available RAM minimum during measured workload | 2,550.8 MiB |
| Outstanding instances / studies after recovery | 0 / 0 |

Three studies exceeded their frozen size deadlines: a medium DX took 10.74 s
against 10.65 s; a small DX took 91.06 s against 6.41 s; and a medium US took
75.50 s against 25.05 s. The last two waited behind a 2,400-instance CT burst.
Instance p99 queue wait was approximately 111.16 s, while p99 time from sender
start to completion was approximately 0.89 s. These are distinct distributions,
not additive percentile components. The evidence points to the scheduled study
bursts sharing four associations as the dominant delay; it does not demonstrate
that increasing Relay RAM would fix it. Assertions and workload were not relaxed.

Reproduction evidence is under `dist/perf-aws-c6a-fixed/attempt-1`, including
`results/performance/point-1-run-1/result.json`, the fixture manifest, network
shaping evidence, resource samples, validation logs and `cleanup.json`.
The measured source archive SHA-256 is
`b587364c8b0fa7573979a8fe8abeb389d242d7ffbddadeecdb93d01b9844f47f`,
based on commit `1e02f8d1b7a3be7fe98a0a8c932e63ada8eeafee` plus the archived
working-tree changes. All required Go checks, including the extraction
permission regression test, passed on the sender before measurement. This
screen does not qualify a published minimum configuration or close issue #4.
The cleanup audit confirms both stacks deleted and zero remaining active
instances, volumes or VPCs; all preceding attempts were also removed.

#### Retest with informational sender queue timing

Run `relay-perf-e32f495eeae9` on 9 September 2026 **passed hardware screening**
with `latencyAcceptanceBasis: started-to-finished`. The VM sizes, resource
allowance, modality mix, arrival rates, network shaping and four associations
were unchanged. This is a change to the screening acceptance clock, not evidence
of a Relay speed improvement. The earlier end-to-end failure remains recorded.

| Measurement | Retest result |
| --- | --- |
| Completed workload | 20 studies, 4,962 DICOM instances, 3,762,979,696 bytes; 20 HL7 messages and 20 confirmed reports |
| DICOM sender queue p99 / maximum (informational) | 111.00 s / 113.30 s |
| DICOM active transfer p99 / maximum | 968.8 ms / 24.66 s |
| DICOM total elapsed p99 (informational) | 111.19 s |
| HL7 active ACK p99 | 89.8 ms |
| Report active completion p99 | 3.15 s |
| Study active transfer maximum | 113.48 s; all size-dependent active-transfer limits passed |
| Peak Relay cgroup memory | 21.86 MiB of the 256 MiB allowance |
| Operation failures / outstanding work / Relay OOM kills | 0 / 0 / 0 |
| Peak sender CPU during calibration | 82.23%; supporting calibration passed |

Evidence is in `dist/perf-aws-transfer-timing/attempt-1`, including the new
`senderQueue` and `transfer` distributions in every summary. The measured source
archive SHA-256 is
`9e8ae231ed0d62779383f0ece56aa76f916cc3c9665e16f20738da5f5638ea84`.
Required race,
vet, publication/licence, interoperability, HL7 fixture, benchmark-smoke and
vulnerability checks passed on AWS, including timing acceptance regression tests.

The initial host package installation was killed before Relay started. A
recorded bootstrap repair used temporary disk-backed swap, then removed it and
disabled the AMI's default zram swap. `host-swap-verification.json` confirms no
host swap before measurement, and Relay samples confirm zero container swap.
Future bootstrap performs this preparation automatically and readiness requires
host swap to be disabled. The repair commands and output are retained alongside
the synthesized templates; the archived source alone does not describe that
manual repair.
The final cleanup audit confirms both stacks deleted and zero remaining active
instances, volumes or VPCs. This remains a short hardware screen, not full
qualification or a published minimum configuration.

## Frozen workload

### Representative modality profile

`screen` defaults to [clinic-mixed-v1](../tools/relay-perf/profiles/clinic-mixed-v1.json).
Other commands retain the original `clinic-v1` profile for reproducibility and
bounded CI. No production Relay configuration or protocol changes are required.

The mixed profile weights **studies**, not instances, frames or bytes:

| Modality | Studies | Small / medium / large pixel bytes | Construction |
| --- | --- | --- | --- |
| US | 40% | 20 / 102 / 495 MiB | 40 stills plus 1 / 2 / 6 multi-frame clips; 8-bit grayscale and RGB; clips have 40 / 48 / 30 frames |
| DX | 35% | 8 / 32 / 96 MiB | 1 / 4 / 6 images; 2048 x 2048 or 2048 x 4096; 16-bit |
| CT | 20% | 100 / 400 / 1200 MiB | 200 / 800 / 2400 images; 512 x 512, 16-bit, multiple series |
| MR | 5% | 50 / 200 / 800 MiB | 400 / 400 / 1600 images; 256 x 256 or 512 x 512, 16-bit, multiple sequences |

Within each modality, the small/medium/large weights are 20/60/20. Selection is
deterministic smooth weighted round-robin, giving exact proportions over 100
studies. The default bounded run contains **20 complete studies** (8 US, 7 DX,
4 CT, 1 MR). Its single MR study cannot cover all three MR sizes. The manifest
records every prepared variant; only the event/result files establish which
variants were actually transferred. Classic MR is used here; enhanced multi-frame
MR, mammography/tomosynthesis, nuclear medicine and fluoroscopy require separate
fixtures and measurements before coverage is claimed.

The weights and size buckets are engineering assumptions informed by public
evidence, not measured clinic percentiles:

- [AIHW 2024-25 Medicare data](https://www.aihw.gov.au/reports/diagnostic-services/pathology-imaging-and-other-diagnostic-services)
  reports approximately 40% US, 36% diagnostic radiography, 16% CT, 5% MR and 3%
  nuclear medicine. Billed services are not DICOM study counts; public hospital
  patients are excluded, and diagnostic radiography includes more than plain DX.
- [VinDr-CXR, Table 2](https://www.nature.com/articles/s41597-022-01498-w.pdf)
  reports 15,000 training radiographs occupying 161 GB, with median dimensions
  2788 x 2446. These are dataset-specific measurements.
- [Ismail and Philbin (2015), Table 1](https://pmc.ncbi.nlm.nih.gov/articles/PMC4479585/)
  includes CT examples containing 338, 1018 and 2524 images, and MR examples with
  277 and 1116 images. These illustrate variation, not current clinical averages.
- [GE LOGIQ S7 conformance](https://www.gehealthcare.com/content/dam/gehc/sitecore-migrated-assets/ultrasound-dicom-conformance-statements/doc1556279_logiq_s7_dicom_conformance_statement_rev3.pdf)
  specifies grayscale, colour and multi-frame ultrasound with configurable
  compression and frame rate.

Study arrivals are independent of available sockets: 120 studies/hour nominal,
150/hour headroom. This is accelerated transfer screening, not an estimate of
patient throughput. HL7 ingest and report arrivals follow the same aggregate
rates; each still uses normal Relay polling and acknowledgement semantics.
Every image in an arriving study is offered together, and its latency includes
waiting behind earlier work. The frozen, bounded metadata queue can hold the
whole schedule; it contains no pixel payloads. Four reusable associations carry
the objects, negotiating the correct storage class when the modality changes.
Repeated archetypes replay their synthetic identifiers and bytes; each transfer
still requires a new cloud receipt.

The 100 Mbit/s, 40 ms cloud RTT, 100 ms receipt delay and 50 ms RIS ACK delay are
unchanged. A universal 30-second study limit cannot apply: 1 GiB alone requires
at least 85.9 seconds at 100 Mbit/s. Mixed instance/study latency budgets are
frozen before the run as follows. Hardware `screen` applies them to active
transfer time; qualification applies them to arrival-to-completion time:

```
5 seconds + transferMargin * (
    study pixel bytes * 8 / bandwidth bits per second
    + study instances * (receipt delay + cloud RTT) / DICOM associations
)
```

`transferMargin` is 2. Every instance includes the study's queue allowance;
upload time and receipt time remain separately measured. The margin is a declared
screening allowance, not a measured service guarantee. Missing work, changed
labels/schedules, payload corruption and premature success still fail the run.
Results include `modalities` summaries keyed by modality/size, actual dataset
bytes, frame counts, storage classes, and per-event deadlines.

Preparation creates about 3.5 GiB of synthetic fixtures in the private workspace,
outside timing. Validated files are then moved into a temporary Docker volume,
one file at a time, to avoid requiring a second whole-corpus copy or streaming
through Docker Desktop's host file-sharing layer during measurement.
Orthanc imports every distinct layout, and dcm4che validates the
explicit test attribute contract and independently decodes/checks every pixel
in the first and last frames. This is not exhaustive IOD certification. The
generator retains at most 128 MiB of fixture files; other files are streamed.
The cloud hashes streams without retaining payloads. Mixed profiles currently
use Explicit VR Little Endian only; the existing JPEG experiment remains a
separate uniform-fixture measurement and is not mixed compression qualification.

Supporting calibration replays the same complete mixed schedule with time
compressed by `2 * headroomFactor`, without intentional network/body throttles.
Concurrent HTTPS health requests establish supporting connections before its
measurement clock starts; Relay connections are not warmed by calibration.
Generator timestamps preserve monotonic elapsed time through JSON export, so
wall-clock adjustments do not create apparent early arrivals.
The default calibration offers nominal work at twice the measured headroom rate
and takes about four minutes plus drain. It must complete every offered object
and report under the declared bounds. Supporting CPU, memory, network and I/O
measurements remain required; saturation makes the result inconclusive.

### Original uniform baseline

The checked-in [clinic-v1 profile](../tools/relay-perf/profiles/clinic-v1.json)
is resolved and saved before traffic starts. A path to a JSON profile may replace
`clinic-v1`; unknown fields and invalid bounds are rejected. Keep custom profiles
under a distinct name and retain their exact resolved contents.

| Setting | clinic-v1 |
| --- | --- |
| DICOM | 8 instances/s; 512 × 1024 8-bit pixels, approximately 512 KiB plus tags per instance |
| Study | 100 distinct instances; approximately 50 MiB |
| DICOM associations | 4 persistent associations; 64 KiB PDUs, fragmented commands and multiple PDVs |
| HL7 ingest | 1 approximately 1 KiB message/s; 4 reusable connections and 16 idle connections; 1 MiB configured limit |
| Returned reports | 1 approximately 4 KiB report/s, serial delivery with normal control polling |
| Network | 100 Mbit/s and 40 ms injected cloud RTT |
| Independent delays | 100 ms after complete upload; 50 ms RIS application ACK delay |
| Qualification phases | 300 s idle, 2400 s nominal, 600 s at 125%, 300 s recovery; three repetitions |
| Acceptance latency | p99 DICOM ≤2 s, HL7 ACK ≤1 s, confirmed report ≤5 s; every study ≤30 s |

The primary syntax is Explicit VR Little Endian. Orthanc imports each synthetic
instance before measurement; the first object is independently IOD-validated and
pixel-decoded with dcm4che. JPEG Lossless SV1 is a separate experiment using actual
Orthanc encoding and dcm4che decoding, with encoded sizes and SHA-256 digests in
its manifest. The image digests are pinned in `fixtures.go` and recorded with each
run. Later synthetic study transfers replay the fixture set as new arrivals;
Relay must obtain a fresh receipt even for repeated SOP instances. Study latency
runs from the first scheduled instance to the last completed instance, including
partial final studies. This measures Relay transfer completion, not cloud study
assembly or a clinical workflow.

The generator schedules arrivals independently of available connections. Full
queues, missed or duplicate arrivals, cancellation, and incomplete work are
visible failures. Latency begins at the scheduled arrival, including sender queue
wait. The cloud hashes streamed datasets and validates exact HL7/report bytes;
it retains measurements and pending reports, not copies of DICOM upload bodies.
Generator inputs have a bounded 128 MiB prefetch cache outside Relay's allowance.
Report completion is observed at Relay's next serial poll after the result was
committed. This proves Relay accepted the cloud response and conservatively adds
one control round trip to the measured confirmation latency.

## Isolation and verdicts

The target is built with the existing release `Dockerfile`, without race,
coverage, or profiling instrumentation. Its ordinary credential watcher, status
writes, polling, and health check remain enabled. Cloud, RIS, generator, observer,
and network helpers have separate containers and no share of Relay's quota.
Only the network helpers receive `NET_ADMIN`. For native targets, an additional
transparent TCP hop preserves end-to-end TLS and uses the same two shaped cloud
queues. This avoids changing native networking or requiring IFB drivers. The hop
is included in supporting-system calibration and observation.

Relay receives `--cpus`, `--memory`, and an equal `--memory-swap` value, which
disables container swap. The observer checks effective `cpu.max`, `memory.max`,
`memory.swap.max`, current/peak memory, CPU accounting and OOM counters before
accepting a result. This follows Docker's
[resource constraint semantics](https://docs.docker.com/engine/containers/resource_constraints/).
Both cloud directions receive half the injected RTT and the configured bandwidth
limit. Cloud body-consumption throttling (`bodyBytesPerSecond`) and post-upload
receipt delay (`receiptMillis`) are separate controls. Exported `tc` evidence
records the effective shaping configuration.

Qualification requires all offered work to complete with exact payloads, fresh
receipts, correlated application ACKs and confirmed report results. It rejects
an OOM, unexpected process failure, premature acknowledgement, unbounded queue
wait, or incomplete recovery. Peak cgroup memory must stay strictly below 80% of
the allowance. First and final ten-minute nominal p95 RSS windows may grow by at
most `max(8 MiB, initial p95 RSS × 0.10)`; missing windows are inconclusive.
Full qualification also rejects gaps over five seconds in Relay observation or
ten seconds in each supporting role, including calibration and fault phases.

Before the target starts, a 30-second direct cloud/RIS calibration offers twice
the headroom workload. This capacity check precedes network shaping and disables
intentional body-consumption throttling; its resolved settings are recorded in
metadata. Calibration uses at least 32 workers per traffic type, independently of
Relay's configured connection count. The configured upload backpressure is enabled for measured Relay
traffic. Supporting CPU, memory, I/O, network and process counts
are sampled during calibration, traffic and faults. Failed calibration, missing resource
measurements or observed supporting saturation makes qualification INCONCLUSIVE;
all observed protocol and latency failures remain in the evidence. An OOM,
unexpected target exit or cleanup failure still fails the run. Resource pressure
on the Docker host also invalidates a run. Inspect generator queue delay and the raw support samples when
interpreting a suspected bottleneck. A PASS from smoke deliberately makes no
performance claim.

Fault checks use normal 3-second idle polling, report retry behavior, 20-second
RIS ACK deadlines and 60-second claims. They cover immediate/delayed/negative,
missing and malformed ACKs, lost result responses and expired claims. A lost
result response must retry the identical cloud result with one RIS send; a new
claim must resend identical report bytes and MSH-10. DICOM faults separately use
5-second idle and 15-second lifetime limits, with five seconds of recovery slack.
Repeated disconnects, aborts, malformed PDVs, invalid contexts, unexpected commands
and stalled uploads must release capacity and permit a subsequent receipt-backed
C-STORE. A still-active upload blocks the next case; that precondition failure
is recorded separately from executing that case. No service restart is accepted
as proof of upload cancellation.

## Separate experiments

```bash
go run ./tools/relay-perf experiment --case jpeg --out dist/perf-jpeg
go run ./tools/relay-perf experiment --case small --out dist/perf-small
go run ./tools/relay-perf experiment --case large --out dist/perf-large
go run ./tools/relay-perf experiment --case associations-32 --out dist/perf-associations-32
go run ./tools/relay-perf experiment --case max-connections --out dist/perf-max-connections
go run ./tools/relay-perf experiment --case max-hl7 --out dist/perf-max-hl7
go run ./tools/relay-perf experiment --case backpressure --out dist/perf-backpressure
go run ./tools/relay-perf fault-check --out dist/perf-faults
```

Also run `associations-1`, `associations-4`, and `associations-8`. `small` uses
100 approximately 32 KiB instances per study; `large` uses 64 MiB pixel objects,
one every 30 seconds, with a declared 30-second instance / 60-second study limit.
`max-connections` uses 128 DICOM and 128 HL7 connections, 128 approximately 32 KiB
DICOM instances/s and 128 approximately 1 KiB HL7 messages/s. Report its supported
rate and actual outstanding work independently of clinic-v1. Connection slots do
not imply that every connection is simultaneously sending. `max-hl7` uses 8 MiB
messages every eight seconds with the configured 8 MiB limit; the normal profile
and benchmarks cover the 1 MiB limit. `backpressure` consumes each cloud body at
256 KiB/s. These are distinct experiments, and can legitimately fail the clinic
latency or supporting-calibration checks. Copy a profile to declare a different
experimental offered rate; do not silently lower offered work after a failure.

## Native Linux and Windows qualification

Use separately provisioned **Linux x64** and **Windows x64** machines, and a
separate Linux-container supporting host. Record processor model, architecture,
OS version, machine RAM, available disk, and background services. Synchronize the
clocks and record the baseline physical network RTT; the helper adds 40 ms, so
physical latency remains additional. Use concrete IPv4 addresses for both hosts; the test shapers validate IPv4 queues.
No command here provisions a machine.

On the supporting host:

```bash
go run ./tools/relay-perf external --profile clinic-v1 \
  --relay-host NATIVE_HOST --support-host SUPPORT_HOST --out dist/perf-native-linux
```

The harness prints a private `native-setup` directory and waits up to five minutes
for the service to poll. On the disposable native host, install the uninstrumented
release artifact from the same source revision using the existing native
installation procedure. While stopped, place the supplied `relay.json` and
`relay-credential.json` at its managed state paths, preserving the documented
service ownership/ACLs. The generated configuration is already paired to the
synthetic cloud. Allow test ingress 11112/2575 and outbound access to supporting
HTTPS 8443 and RIS 2576. The supporting admin port remains loopback-only.

Trust only this run's ephemeral CA on the disposable host: Linux services can set
`SSL_CERT_FILE` to the supplied CA; Windows services need it in the machine trusted
root store. Remove that trust and restore/retire the test service afterward. The
harness deletes its private setup during cleanup, so transfer it during the wait.
Each repetition receives fresh setup material and requires the service restart.
Never export these credentials or the CA private key with the evidence.

Observe the **clinical Relay process**, separately from its native privileged
management broker. Run with permission to read process counters; record the broker
PID/resource consumption independently if it is present, and include it in total
machine measurements. `observe-native` accepts only a Relay binary, records its
SHA-256, Go build settings and hardware identity, and rejects race/coverage builds:

```bash
go run ./tools/relay-perf observe-native \
  --pid CLINICAL_PID --duration 70m --out native-run-1.jsonl
```

Start observation before traffic and repeat for each restarted process. Windows
uses process working set/CPU/I/O and system counters; Linux uses `/proc`. Preserve
the companion `.metadata.json`. Combine these measurements with the corresponding
external run by UTC timestamps. Require continuous coverage of idle, load and
recovery, verify source/build identity, calculate the same initial/final ten-minute
RSS windows, and report Relay and total machine resources independently. Native
process measurements are not cgroup measurements: do not label machine RAM as a
Relay hard limit or infer Windows memory from Docker's Linux VM.

Run `external-fault-check` separately with the same two host arguments and a fresh
output directory. Install the newly generated fault configuration with its
explicit shortened DICOM timeouts. Retain its fault evidence alongside all three
full runs. The external result deliberately remains INCONCLUSIVE until native
resource, fault, disk and lifecycle evidence has been reviewed together.

On each disposable native host, collect uninstrumented lifecycle evidence using
the existing installed tests. These commands intentionally install and replace
the test service and approve the exact synthetic signed candidate within the
existing lifecycle assertions. They do not authorize a production update.

Linux:

```bash
TELRAD_NATIVE_INSTALL_TEST=1 \
TELRAD_PERF_LIFECYCLE_OUT="$PWD/native-lifecycle.json" \
  scripts/check-native-installation.sh
```

Windows (elevated PowerShell):

```powershell
$env:CGO_ENABLED = '0'
$env:TELRAD_NATIVE_INSTALL_TEST = '1'
$env:TELRAD_PERF_LIFECYCLE_OUT = Join-Path $PWD 'native-lifecycle.json'
./packaging/install-native.Tests.ps1
```

Leave `RELAY_NATIVE_COVERAGE_DIR` and `GOCOVERDIR` unset for these measurements.
The evidence records startup/pairing, restart, exact signed update and automatic
rollback durations, installation disk samples and remaining space. Supplement it
with native machine observation during the lifecycle run and retain the tested
artifact identities, whose SHA-256 and Go build settings are included. Windows Authenticode/distribution verification remains the
existing separate release validation; these synthetic update tests alone do not
establish production signing readiness.

Only after three complete passing runs **on each OS**, successful separate fault
and lifecycle experiments, and adequate supporting capacity may the evidence
report publish a minimum tested configuration. Update README with the measured
processor, architecture, OS, machine RAM, Relay allowance if enforced, disk
headroom, workload, connection counts and achieved performance. Retain raw evidence
and use the same selected profile for subsequent release validation. No ARM,
production-cloud capacity, publication or provisioning claim follows from this.

## Benchmarks and diagnostics

```bash
go test ./cmd/telrad-relay -run '^$' \
  -bench '^(BenchmarkReadMLLPFrame|BenchmarkHL7PersistentMixed|BenchmarkHL7Stream|BenchmarkDICOMParsing|BenchmarkDICOMStream)$' \
  -benchmem -count=3
# These intentionally use real polling/ACK/claim timers; expiry takes a minute.
go test ./cmd/telrad-relay -run '^$' -bench '^BenchmarkReportReturn$' \
  -benchtime=1x -benchmem -count=3 -timeout=15m
# Separate diagnostic run, never qualification evidence:
go test ./cmd/telrad-relay -run '^$' -bench '^BenchmarkDICOMStream$' \
  -benchtime=3x -cpuprofile=dist/perf-cpu.pprof -memprofile=dist/perf-heap.pprof
```

HL7 payload size and configured limit vary independently. Metrics include frame
capacity and per-connection reader storage; mixed coalesced frames reuse each
worker's reader. DICOM parsing covers 16/64 KiB PDUs and command fragments. Stream
benchmarks exercise complete HL7 HTTPS/ACK exchanges on persistent concurrent
connections and real Relay C-STORE receipt handling over 1/100-instance
associations. Fixtures are constructed before timing. Whole-exchange allocations
include the synthetic sender/cloud and cannot be interpreted as isolated Relay
allocation counts. Compare repeated baselines on the same otherwise idle host.

For a separate full-runtime diagnostic, run `TestPerformanceDiagnostic` with
`TELRAD_PERF_CONFIG` pointing to an external harness's synthetic configuration,
`TELRAD_PERF_DURATION` to a bounded duration, and `TELRAD_PERF_METRICS` to a new
JSONL path. Supply the synthetic CA via the native trust mechanism. Use Go test's
`-cpuprofile` and `-memprofile`; runtime samples include heap, allocations, GC and
goroutine counts through shutdown. Compare idle, load and recovery windows to
explain retained memory or goroutines. Diagnostic binaries cannot establish
minimum requirements. For example, a separate Linux diagnostic run can use:

```bash
SSL_CERT_FILE=/path/to/synthetic/ca.pem \
TELRAD_PERF_CONFIG=/path/to/synthetic/relay.json \
TELRAD_PERF_DURATION=70m TELRAD_PERF_METRICS=dist/perf-runtime.jsonl \
  go test ./cmd/telrad-relay -run '^TestPerformanceDiagnostic$' -count=1 \
  -timeout=75m -cpuprofile=dist/perf-runtime-cpu.pprof \
  -memprofile=dist/perf-runtime-heap.pprof
```

There is no production profiling endpoint. See the
[Go profiling documentation](https://pkg.go.dev/runtime/pprof).

## Evidence and current status

Each run saves `profile.json`, sanitized `fixtures.json`, `relay-config.json`, `metadata.json`,
`calibration.json`, `traffic.json`, `result.json`, shaping evidence, fault results
when requested, and bounded helper logs. `result.json` schema version 1 contains
source/image identities, dirty-tree status, dependency digests, host metadata,
raw events, offered/started/completed/failed/outstanding counts, p50/p95/p99/max
latencies, backlog, throughput, CPU/RSS/cgroup samples (including separate fault-phase observations), support measurements,
control polling, retry counts, faults and cleanup outcomes. Upload duration,
post-upload receipt delay and whole-study completion are distinct measurements.
Artifacts under `dist/` are local and ignored by Git; CI retains its bounded
correctness artifacts. Exported evidence contains synthetic sequence numbers and
hashes, not payloads, credentials, DICOM instance/study UIDs, HL7 control IDs or claim tokens.

Initial implementation evidence on 2026-09-08 used source main
`1e02f8d1b7a3be7fe98a0a8c932e63ada8eeafee` plus the recorded uncommitted harness
changes. The supporting environment was Docker Desktop 29.4.3, Linux x86_64,
cgroup v2, 8 visible CPUs and approximately 3.82 GiB RAM. The 1 CPU/256 MiB bounded
smoke passed correctness and resource-enforcement checks. This is not a minimum
configuration result.

The separate fault run passed all seven report cases. Lost result response:
one RIS send, two result requests. Expired claim: two RIS sends after approximately
60 seconds. A slow-progress DICOM upload failed after approximately five seconds
and remained active, preventing clean setup of the remaining DICOM cases. The
harness reports that failure; it neither fixes #16 nor declares qualification
complete. Native Linux/Windows hour-long runs, installed lifecycle measurements,
and full-duration experiments remain to be executed on supplied hosts.

The local diagnostic baseline (`dist/perf-benchmarks.txt`, three repetitions at
three iterations each) recorded 32,768 B/op and one allocation for a 1 KiB HL7
frame under either configured limit. A 1 MiB payload retained approximately
1 MiB at the 1 MiB limit and 2 MiB at the 8 MiB limit. These allocation/capacity
observations motivate separate optimization work; the short profiled timing
samples are not throughput qualification. CPU and heap diagnostic artifacts are
`dist/perf-cpu.pprof` and `dist/perf-heap.pprof`.

A short resource screen (`sweep --duration 15s`, output
`dist/perf-screening`) passed at 2 CPUs/512 MiB with approximately 15.97 MiB peak
cgroup memory. The next 1 CPU/512 MiB point was INCONCLUSIVE **before Relay
started**: supporting calibration DICOM p99 was approximately 2.253 seconds,
including approximately 2.052 seconds of p99 sender queue wait. All 600 offered
calibration DICOM instances completed, but the supporting latency criterion did
not pass. Screening correctly stopped; the smaller points were not attempted.
This result does not establish a Relay failure at 1 CPU.

The final bounded CI-equivalent rerun (`dist/perf-final-smoke`) passed with a normal
release target and coverage confined to helpers. It completed 53/53 DICOM
instances, 7/7 HL7 messages and 7/7 reports; peak cgroup memory was 14,053,376 bytes.
The observed p99 values (DICOM 392 ms, HL7 63 ms, reports 2,988 ms) describe this
short correctness run and are not used as qualification thresholds in CI. The
separate Docker external-network test also passed after replacing the unsupported
IFB interface with a TLS-transparent hop using the existing shaped queues.

Short exploratory experiments (`dist/perf-experiments`, 15-second nominal load,
2 CPUs/512 MiB) exercised JPEG Lossless SV1, 64 MiB objects, maximum HL7 size,
1/4/8/32 associations, maximum connections, small instances and upload
backpressure. JPEG, maximum HL7, 4/8/32 associations and the partial small-instance
study passed those short checks. That initial small-object profile used 1000
instances per study; the final experiment keeps clinic-v1's 100-instance study
size so changing object size does not impose an inherently impossible study
latency at eight arrivals per second. Backpressure exceeded DICOM queue, instance and
study latency bounds. The large-object run also exceeded HL7/report latency while
the supporting host was saturated; it cannot establish target capacity. Maximum
connections did not pass support calibration, so no supported maximum-concurrency
workload was established. The initial one-association calibration exposed an
incorrect coupling to target connection count; calibration now uses independent
support workers. These exploratory artifacts predate the final confirmation
measurement and verdict refinements and do not establish qualification.

With independent calibration workers, the one-association rerun
(`dist/perf-association-1-final`) passed calibration and then failed DICOM queue
and latency bounds: all 158 instances completed, but p99 was approximately
12.021 seconds. The large-object rerun (`dist/perf-large-final`) passed its short
experimental bounds with both 64 MiB objects complete, DICOM p99 approximately
6.228 seconds, HL7 p99 92 ms and report p99 3.190 seconds. The difference from the
earlier saturated run reinforces the need to retain support observations and
repeat qualification on dedicated hosts.

The final fault artifact (`dist/perf-fault-final`) contains 108 fault-phase Relay
samples and all seven passing report cases, including one RIS send for the
missing-ACK timeout. Its slow-progress failure left one upload active after
approximately 5.145 seconds; the six subsequent DICOM cases could not start.
Separate negative harness checks detected an OOM at a deliberately
undersized 6 MiB limit and exported 1,589 events after controller cancellation.
Both removed their private test resources without cleanup errors. Cancellation
now preserves its original cause instead of a secondary missing-output error.

The final small-object run (`dist/perf-small-final`) passed with 158/158 instances,
including a complete 100-instance study and a final partial study. DICOM p99 was
approximately 198 ms and maximum study completion was 12.524 seconds. The final
CI-style smoke (`dist/perf-validation-smoke`) also passed with all 53 DICOM,
seven HL7 and seven report arrivals complete, and no cleanup errors.

Local validation passed formatting, race tests, vet, publication/licence audits,
Orthanc/dcm4che interoperability, HAPI fixture validation and vulnerability checks.
Linux/Windows test builds compiled, and the TLS-transparent external networking
path passed its Docker integration test. Unit plus helper coverage was 68.6%;
the unchanged 69% CI gate additionally includes installed native-process
coverage. Hosted CI and native lifecycle measurements have not run here.
