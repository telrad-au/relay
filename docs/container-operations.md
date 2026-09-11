# Container operations

The Compose deployment runs an immutable image with a read-only root filesystem,
no additional Linux capabilities, and a named volume for schema-v5 configuration
and its protected credential lifecycle record. The
container does not update its own image. Native privilege helpers and management
IPC are excluded from the image build. `auth`, `enroll`, `rotate-credential`, `doctor`,
`status`, and `ready` operate directly as UID `10001`, without sudo or systemd.
Read-only checks do not perform configuration migration or transaction recovery.
Start and stop the service through the container runtime.

## First pairing

Copy `packaging/compose.env.example` outside the repository and fill in the
immutable image reference, clinic-facing bind address, and report destination.
Do not put `TELRAD_RELAY_PAIRING_TOKEN` in that file.

```bash
cp packaging/compose.env.example relay.env
chmod 600 relay.env
docker compose --env-file relay.env -f packaging/compose.yml config
docker compose --env-file relay.env -f packaging/compose.yml pull
read -rsp 'Pairing token: ' TELRAD_RELAY_PAIRING_TOKEN && printf '\n'
export TELRAD_RELAY_PAIRING_TOKEN
docker compose --env-file relay.env -f packaging/compose.yml run --rm \
  --env TELRAD_RELAY_PAIRING_TOKEN relay enroll
unset TELRAD_RELAY_PAIRING_TOKEN
docker compose --env-file relay.env -f packaging/compose.yml up --detach
```

Relay consumes the token exactly once, removes it from the process environment,
validates only its cloud-defined length, and clears the in-memory bytes after
the pairing request. It never writes or logs the token. The one-off enrollment
container exits after populating `relay-credential.json`; the long-lived
service is created without any token value:

```bash
docker compose --env-file relay.env -f packaging/compose.yml logs --tail 100 relay
docker compose --env-file relay.env -f packaging/compose.yml ps
```

Official images contain the source-controlled production pairing endpoint. Set
`TELRAD_RELAY_PAIRING_URL` only to override that default for development or a
self-hosted control plane. The override must use an approved HTTPS endpoint
whose exact path is `/v1/relay/pairing-enrollments`. Relay derives its fixed
control, DICOM, and HL7 paths locally from that one origin; pairing cannot
select another destination.

Relay automatically migrates a legacy bearer record and renews lifecycle
credentials before access expiry. Each operation is persisted before its HTTPS
request and retried with the same operation ID after a timeout or container
restart. Keep exactly one Relay service instance attached to the volume and do
not edit or copy the credential record between installations.

## Networking

Publish TCP `11112` only to DICOM sources and TCP `2575` only to HL7 sources.
The report destination must be reachable from the container:

- use a routable address for another host;
- use a Compose service name for a receiver on the same Docker network;
- use `host.docker.internal` on Docker Desktop; or
- add `host.docker.internal:host-gateway` on Linux when that topology is
  explicitly chosen.

Outbound HTTPS uses system CA roots and the standard `HTTPS_PROXY` and
`NO_PROXY` variables. Redirects are rejected for all authenticated protocol
requests. Do not install a private CA merely to bypass certificate errors;
review the intended enterprise trust policy first.

## Health and degraded operation

The image health check runs `ready`. It remains healthy during temporary
control-channel loss while DICOM and HL7 ingest remain available. `status`
separately reports control and report-return availability. A cloud `401` or
`403` marks the container unhealthy until re-pairing succeeds. A rejected or
expired renewable credential has the same effect.

Relay streams DICOM uploads directly and does not maintain an ingest spool.
Stopping or recreating the container drains active work for up to one minute;
an interrupted DICOM object is not replayed by Relay.

## Upgrade and rollback

Record the current immutable image reference. Verify the new release, replace
`TELRAD_RELAY_IMAGE`, and recreate the service without a pairing token:

```bash
docker compose --env-file relay.env -f packaging/compose.yml pull
docker compose --env-file relay.env -f packaging/compose.yml up --detach
docker compose --env-file relay.env -f packaging/compose.yml ps
```

Schema v2 cannot run in the HTTPS-v1 image. Before upgrading, use the new
binary's `migrate-config` command against the protected volume. It preserves
non-credential settings and update trust, deletes obsolete certificate and
pending request files, and requires re-pairing. There is no certificate
credential migration or protocol compatibility mode.

Schema v3 upgrades automatically on startup to schema v4 and HTTPS polling,
preserving the credential and listener settings. Coordinate this change with
the Telrad API and edge cutover. Existing ledger files are ignored and can be
removed once the cutover is complete. There is no local report state to restore.
See [native operations](native-operations.md#schema-v3-polling-cutover).

To roll back, restore the recorded immutable image reference only if that
release supports the current configuration schema. The credential volume is
not a substitute for a DICOM or HL7 delivery queue.

## Removal

`docker compose down` keeps the named volume. Removing
`telrad-relay-data` permanently destroys the Relay configuration and credential
and requires a new pairing, so handle that as a separate approved destructive
operation. Never use `docker compose down -v` as an ordinary troubleshooting
step.

## Retrieval configuration

Schema v4 upgrades to v5 with retrieval absent. Optional retrieval keeps only its
permit signing key and approved configuration in the existing volume; it adds no
clinical spool or job state. Generate the key as UID 10001 and follow the
[PACS retrieval guide](pacs-retrieval.md) for qualification and rollback.

## Order-authorized reports

Report authorization is always enabled in both image modes. Relay automatically
creates and retains `report-signing-key.json` beside its credential file; no
additional settings or key-provisioning commands are required. Preserve the
protected state directory across restarts and upgrades. See
[report authorization](report-authorization.md) for the order flow and recovery.
