# Native Relay operations

This guide covers Linux systemd and the Windows service installation.

## Network and listener policy

Relay needs outbound TCP `443` to its configured HTTPS/WSS origin and no
inbound internet rule. Clinic systems connect only to the scoped local firewall
rules for DICOM TCP `11112` and HL7 TCP `2575`. Report return defaults to
`127.0.0.1:2576`.

`listenAddress` must be an explicit IP address. The packaged `0.0.0.0` default
requires host-firewall restrictions to the modality, PACS, and RIS source
addresses. The default limits are 256 clinic connections globally, 128 per
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
`%ProgramData%\Telrad\Relay`. Configuration schema `3` contains endpoint and
listener settings but no bearer value. `relay-credential.json` is protected by
Unix mode `0600` under a `0700` directory or by the installer-managed Windows
service ACL.

The managed executable and update trust are administrator-owned. Linux stores
them under `/usr/local/lib/telrad-relay`; Windows stores them under
`%ProgramFiles%\Telrad Relay`. The service identity receives read/execute access
but cannot replace the executable or change the trusted update key.

Run `telrad` after installation; `telrad auth` is the explicit equivalent. Both
authenticate the host when needed and start the service. Use `telrad enroll`
only to authenticate the host again. Relay displays a Telrad browser-approval
URL and waits for an authorized clinic administrator. It keeps the device
secret out of the URL, polls the configured HTTPS origin at the server-provided
interval, and immediately redeems the short-lived pairing token returned after
approval. Pairing verifies the returned Relay ID, credential grammar, protocol
version, and content type, then derives the fixed control, DICOM, and HL7 paths
locally from the configured pairing origin before a journaled two-file commit.
For rollout compatibility, legacy returned endpoint fields are accepted only
when every value exactly matches the locally derived destinations. The device
secret and pairing token are never persisted or logged. Do not move a
credential record between hosts; re-pair a replacement host.

To rotate on demand:

```bash
telrad rotate-credential
```

The file update is atomic. If the server provides an overlap window, the prior
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

The command does not download an executable or change the host. Review the
printed source and release for that exact version. Then approve only that
version from an Administrator terminal (or with `sudo` on Linux):

```bash
telrad update VERSION
```

Relay refuses the request if `VERSION` no longer matches the signed manifest,
is already installed, or would be a downgrade. It also requires the running
Relay to be ready before beginning. The approved artifact is independently
verified with Ed25519 and SHA-256, then Relay stops accepting new work, drains
active exchanges, stops the service, transactionally replaces the executable,
starts the service, verifies the exact new version is ready, and rolls back on
failure. There is a brief ingest interruption during replacement; this is not a
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
