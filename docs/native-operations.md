# Native Relay operations

This guide covers Linux systemd and the Windows service installation.

## Network and listener policy

Relay needs outbound TCP `443` to its configured HTTPS origin and no
inbound internet rule. Clinic systems connect only to the scoped local firewall
rules for DICOM TCP `11112` and HL7 TCP `2575`. Report return defaults to
`127.0.0.1:2576`.

`listenAddress` must be an explicit IP address. The packaged `0.0.0.0` default
requires host-firewall restrictions to the modality, PACS, and RIS source
addresses. Windows installation creates missing Relay firewall rules and preserves
existing operator rules, including their scope and enabled state. The installer's
`ClinicRemoteAddress` option applies when creating rules. The default limits are 256 clinic connections globally, 128 per
protocol, a five-minute DICOM idle timeout, a two-hour DICOM lifetime, and no
Relay-enforced HL7 idle or lifetime timeout. HL7 messages default to 1 MiB and
cannot be configured above 8 MiB.

Outbound requests use system trust, HTTP keepalive, environment proxy
resolution, a 10-second connect timeout, a 15-second TLS handshake timeout, and
a 30-second response-header timeout. A systemd service does not inherit an
interactive shell's environment; configure reviewed `HTTPS_PROXY` and
`NO_PROXY` values in a protected service environment file. Proxy credentials
are secrets and must not appear in tickets or logs.

## Configuration and pairing

Linux stores configuration and credentials in `/etc/telrad-relay`; Windows uses
`%ProgramData%\Telrad\Relay`. Configuration schema `4` contains endpoint and
listener settings but no bearer value. `relay-credential.json` is protected by
Unix mode `0600` under a `0700` directory or by the installer-managed Windows
service ACL.

The managed executable and update trust are administrator-owned. Linux stores
them under `/usr/local/lib/telrad-relay`; Windows stores them under
`%ProgramFiles%\Telrad Relay`. The service identity receives read/execute access
but cannot replace the executable or change the trusted update key.

Run `telrad` after installation; `telrad auth` is the explicit equivalent. Both
start the service when needed and authenticate the host through local management. Use `telrad enroll`
to authenticate the host again; standalone enrollment and rotation require the
service to be running. Relay displays a Telrad browser-approval
URL and waits for an authorized clinic administrator. It keeps the device
secret out of the URL, polls the configured HTTPS origin at the server-provided
interval, and immediately redeems the short-lived pairing token returned after
approval. Pairing verifies the returned Relay ID, credential grammar, protocol
version, and content type, then derives the fixed control, DICOM, and HL7 paths
locally from the configured pairing origin before a journaled two-file commit. Pairing recovery uses only locally derived
managed filenames; malformed journals, unexpected credential paths, links, and
reparse points require administrator repair and are never followed.
For rollout compatibility, legacy returned endpoint fields are accepted only
when every value exactly matches the locally derived destinations. The device
secret and pairing token are never persisted or logged. Do not move a
credential record between hosts; re-pair a replacement host.

To rotate on demand:

```bash
telrad rotate-credential
```

Administrator authorization applies to this exact operation; the service performs
the HTTPS request and credential update under its own identity. The file update is atomic. If the server provides an overlap window, the prior
credential remains only until its deadline. A running Relay adopts the new
current credential within one second and reconnects control without rebinding
the clinic listeners. Rotation is not periodic.

## Status and degraded behavior

```bash
telrad status
telrad doctor
telrad ready
```

`status` prints four separate signals:

- ingest ready;
- control connected;
- report return available; and
- authentication attention.

`ready` requires a fresh status record, bound ingest listeners, and healthy
authentication. Temporary control loss does not make ingest unready, although
report return is unavailable until control reconnects. An observed `401` or
`403` requires a credential replacement or re-pairing.

Linux logs are available through `journalctl --unit telrad-relay`; Windows uses
the Application event log. Logs intentionally omit device secrets, pairing
tokens, bearer credentials, authorization values, HL7 idempotency keys, DICOM
UIDs, HL7 control IDs, message bodies, and cloud response bodies. Support
records should contain only versions, bounded opaque IDs, byte counts,
durations, outcomes, and error codes.

## DICOM and HL7 delivery behavior

The DICOM SCP accepts the called AE `TELRAD`, negotiates one supported transfer
syntax per presentation context, and supports C-ECHO plus sequential C-STORE.
It writes a deterministic Part 10 header and streams unchanged dataset PDVs to
HTTPS with a 1 GiB total cap. There is no spool, transcode, or internal replay.
Each C-STORE is one distinct HTTPS request without an `Idempotency-Key`; Relay
does not compare SOP Instance UIDs, checksums, or bytes with prior stores.
Success is returned only for HTTP `201` with an accepted receipt containing a
valid receipt ID. Network failures, `408`, `429`, and retryable `5xx` responses
map to `0xA700`; malformed objects and permanent content rejection map to
`0xA900`; malformed cloud success responses map to `0xC000`. Authentication
failure marks attention, returns failure, and aborts the association. Relay
never retries the consumed stream: a PACS retry is deliberately a new arrival
whose definitive or non-definitive classification belongs to the cloud.

The MLLP listener validates UTF-8 and `MSH-10`, keeps the clinic connection open
for sequential exchanges, and returns the exact correlated cloud ACK.
Retry-eligible delivery failures use at most three attempts over 60 seconds with
the same body and idempotency key. Other failures close the clinic exchange
without a synthetic ACK.

Report return uses authenticated HTTPS polling every three seconds while idle,
and polls immediately after work. Telrad owns the durable delivery queue and
issues one 60-second claim at a time. Relay sends the original HL7 message to
the RIS and requires a correlated application `AA` before reporting success.
The active result stays in memory and is retried until Telrad confirms its
commit or the claim deadline expires. Graceful shutdown drains that attempt.

Relay has no report delivery ledger. If a report reaches the RIS but its result
never reaches Telrad, Telrad can send the identical payload and `MSH-10` again.
The RIS must tolerate that duplicate without duplicate clinical effects.
An unexpected process exit loses the in-memory result; cloud lease expiry
provides recovery without manual reconciliation of local state.

## Schema-v3 polling cutover

Upgrade Telrad's API, edge routing and all Relay installations together. The
old WebSocket control endpoint is removed. Allow report attempts to drain,
stop old Relays, deploy the API and edge, then start the new Relays. Ingest
payload protocols and enrolled credentials remain unchanged.

Loading a valid schema-v3 configuration automatically writes schema v4,
changing the derived control URL from WSS to HTTPS while preserving credentials,
listener settings and update trust. Native installers also perform this upgrade.
Existing `report-delivery-ledger.json`, `.db`, and recovery copies are ignored;
they can be removed after the cutover. They are never read or migrated.
Older binaries reject schema v4. A rollback across this cutover requires restoring
the matching API and saved configuration together; do not downgrade a running
installation or reuse stale ledger state.

## Schema-v2 hard cutover

Stop the service and run the installer-provided migration or:

```bash
telrad --config PATH migrate-config
```

Migration preserves listener settings, connection limits, report routing,
timeouts, and Ed25519 update trust. It removes the old identity fields, clears
paired URLs and Relay ID, sets `credentialPath`, and deletes the obsolete
runtime identity and pending request files only after the new configuration is
durable. The service remains disabled and must be re-paired. No legacy route,
certificate migration, or compatibility mode is available.

## Updates and recovery

Checking is deliberately separate from applying:

```bash
telrad update
```

For a stable installation this fetches the stable manifest directly. For a
testing installation it uses the public GitHub Releases feed to locate the
newest immutable `testing-*` manifest. The feed is discovery only: Relay
accepts the candidate only after its isolated channel signature verifies. The
signed metadata binds the channel, version, release tag, source commit,
platform, artifact URL, and SHA-256.

The check does not download an executable, load credentials, recover transactions,
migrate configuration, or change the host. Review the
printed source and release for that exact version. Then approve only that
version from an ordinary terminal; Relay requests sudo/UAC for the restricted
installation action:

```bash
telrad update VERSION
```

Relay refuses the request if `VERSION` no longer matches the signed manifest,
is already installed, or would be a downgrade. It also requires the running
Relay to be ready before beginning. The approved artifact is independently
verified with Ed25519 and SHA-256, then Relay stops accepting new work, drains
active exchanges, stops the service, transactionally replaces the executable,
starts the service, verifies the exact new version and service-owned diagnostics,
and rolls back on failure. The CLI downloads the candidate before requesting
elevation. The restricted installer independently rechecks its signature, channel,
platform, digest, and exact version against protected trust. The trusted installer
owns the transaction; the command waits for readiness or completed rollback on
both Linux and Windows. There is a brief ingest interruption during replacement; this is not a
zero-downtime upgrade.

Relay does not poll for or automatically apply releases. The administrator-owned
trust file and executable are outside the service identity's writable paths.
Stable and testing installations pin different public keys and expected
channels. They cannot cross channels through `telrad update`; doing that
requires a separately reviewed installer.
HTTPS update downloads may follow redirects because they are unauthenticated
and independently signature-verified; authenticated Relay requests never do.

During shutdown or an approved update restart, Relay stops accepting new work
and drains existing exchanges for the configured service grace period. An
interrupted streamed DICOM upload must be resent by the originating system;
Relay does not retain a recoverable copy.

## Local management and privilege boundaries

Ordinary `version`, `status`, `doctor`, `ready`, and update checks do not elevate.
Read-only checks never recover pairing state or migrate an old configuration.
If the service is stopped, `status` still queries the OS, while service-owned
credential diagnostics report that they are unavailable. `ready` fails until the
clinical runtime is ready.

The native service can run unpaired with only local management available. It
opens DICOM and HL7 listeners after successful pairing. Re-pairing drains active
work before replacing the live identity. Disconnecting during that drain resumes
the previous identity; rotation preserves live credential
adoption without rebinding listeners.

Linux uses `/run/telrad-relay/management.sock`, authenticating OS peer credentials.
Windows uses local named pipes with separate diagnostic and administrator access.
Clients verify the server identity; Windows administrator clients permit
identification only, preventing the service from borrowing their elevated token.
No management TCP port is opened. The daemon keeps its existing dedicated
identity and Linux `NoNewPrivileges=true` setting.

The CLI announces the exact administrator action even when sudo/UAC authorization
is cached. A restricted native entry point handles only fixed service actions,
local credential-operation authorization, migration, and signed update installation.
It accepts no configuration paths or service-selected executable targets.
Custom `--config` commands operate with the caller's existing permissions and
cannot request managed update application.

Installation metadata (`installation.json`), staged updates, update journals, and
rollback copies live beside the administrator-owned executable. The old
service-writable `installation.json` is ignored. Managed credentials must remain
at `relay-credential.json` in the configuration directory; equivalent absolute
paths are normalized. Other managed layouts require explicit repair before upgrade.

All installer variants use the same native implementation for fixed-target writes,
permissions, migration, and rollback. Installer repairs touch only named managed
files and preserve existing enrollment and deliberately stopped services. A failed
installation restores the previous service configuration and CLI/enablement links,
and removes any firewall rules, PATH entry, or event source it just created.
Windows backup files receive private ACLs at creation, before their contents are
written. Directory access rejects links in any path component. Fresh
installations start with local management available. Legacy schema-v2 cleanup
removes only known Relay certificate filenames, never paths supplied by the old
configuration. Unexpected custom legacy files require separate administrator cleanup.
An installation with pending pairing state must first let its existing service
complete recovery. An invalid journal requires administrator repair; the installer
leaves it untouched.

An interrupted updater leaves its protected transaction evidence in place and
refuses another application. Repair it with a reviewed native installer; do not
edit a journal to nominate recovery paths or execute a staged file manually.
