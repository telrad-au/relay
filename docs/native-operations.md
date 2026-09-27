# Native Relay operations

This guide covers the Linux systemd service and the Windows service. See
[architecture](architecture.md) for the design and the protocols Relay speaks
to Telrad.

## Network and listener policy

Relay needs no inbound internet rule. Its outbound connections are:

- TCP to Telrad's DICOM, HL7 and report ports, using TLS with Relay's client
  certificate. Pairing supplies the host and ports; they are not configuration.
  These connections are made directly and do not use an HTTP proxy.
- HTTPS on TCP `443` to the enrolment endpoint, for pairing and renewal. These
  requests honour the standard `HTTPS_PROXY` and `NO_PROXY` variables. A
  systemd service does not inherit an interactive shell's environment, so set
  proxy variables for the service itself. Proxy credentials are secrets.

On the clinic network Relay listens on DICOM TCP `11112` and HL7 TCP `2575`,
and connects to the report receiver at `reportHost`:`reportPort`. The report
receiver connection is plain MLLP on the clinic network.

`listenAddress` must be an explicit IP address. The default `0.0.0.0` listens
on every interface, so restrict the two ports in the host firewall to the PACS
and RIS source addresses. The status endpoint listens on loopback only.

Relay verifies Telrad's certificates with the operating system's trust store,
requires TLS 1.2 or later, and ships no trust material of its own.

## Configuration

| Platform | Configuration file | Data directory |
| --- | --- | --- |
| Linux | `/etc/telrad-relay/relay.json` | `/var/lib/telrad-relay` |
| Windows | `%ProgramData%\Telrad\Relay\relay.json` | `%ProgramData%\Telrad\Relay` |

`relay.json` uses schema version `6`. Unknown fields are rejected, and a file
with any other `schemaVersion` is refused; there is no migration from earlier
schemas. Change the file and run `telrad restart` to apply it.

The installer writes `relay.json` only when it is absent and never changes it
on a reinstall. On that first install it asks on the terminal for the report
receiver host and port (default `2576`), re-asking until the answer is valid,
or takes them from `TELRAD_RELAY_REPORT_HOST` and `TELRAD_RELAY_REPORT_PORT`
(`-ReportHost` and `-ReportPort` on Windows). With no terminal and no host
given, or when the question is left empty, it writes the placeholder
`report-receiver.invalid` and prints a warning: Relay still pairs and forwards
orders, but every report is answered `AE` and retried by Telrad until
`reportHost` is set and the service restarted.

| Field | Default | Meaning |
| --- | --- | --- |
| `schemaVersion` | `6` | must be `6` |
| `enrolmentUrl` | built in | Telrad enrolment endpoint; HTTPS only |
| `dataDir` | platform default | identity and ledger; absolute path |
| `listenAddress` | `0.0.0.0` | clinic-facing bind address |
| `dicomPort` | `11112` | clinic DICOM listener |
| `hl7Port` | `2575` | clinic HL7 listener |
| `reportHost` | required | clinic report receiver host |
| `reportPort` | `2576` | clinic report receiver port |
| `statusAddress` | `127.0.0.1:8425` | local status endpoint; loopback only |
| `maxDicomConnections` | `128` | concurrent DICOM connections |
| `maxHl7Connections` | `128` | concurrent HL7 connections |
| `hl7MaxBytes` | `1048576` | largest HL7 message; at most 8 MiB |
| `hl7FrameSeconds` | `30` | time allowed to finish a started MLLP frame |
| `connectTimeoutSeconds` | `10` | dial timeout for Telrad and the report receiver |
| `telradAckSeconds` | `60` | wait for Telrad's acknowledgement of a forwarded order |
| `receiverAckSeconds` | `30` | wait for the report receiver's acknowledgement |
| `idleTimeoutSeconds` | `900` | close a clinic connection idle this long |

Every field can be overridden by an environment variable named
`TELRAD_RELAY_` followed by the field name in upper snake case, for example
`TELRAD_RELAY_REPORT_HOST` or `TELRAD_RELAY_HL7_MAX_BYTES`. Environment
overrides are intended for containers; on a native install, prefer the file.
`TELRAD_RELAY_ENROLMENT_URL` is for development only.

The data directory holds two files, both readable only by the service account:

- `identity.json`: the private key, certificate, Relay identifier and Telrad's
  host and ports;
- `accessions.ledger`: the accession numbers of accepted orders, one per line.

Do not copy either file to another host. Pair a replacement host instead.

## Pairing

The service starts unpaired, with only the status endpoint open. It generates a
key, asks Telrad for a pairing request and publishes the verification link on
its status endpoint. Run:

```bash
telrad
```

`telrad` prints the link. An authorised person opens it, signs in to Telrad,
chooses the company and approves. The service polls Telrad until approval,
stores its identity, opens the DICOM and HL7 listeners and starts report
pickup. An expired or refused link is replaced by a new one; run `telrad` again
to see it. Pairing requests never follow redirects.

## Status and degraded behaviour

```bash
telrad status
```

`telrad` with no command does the same. It reads the service's local
`/status` endpoint and prints:

- the state: `pairing` until paired, `ready`, or `degraded`;
- the report receiver as `host:port`, or `report receiver: NOT CONFIGURED`
  with the file to edit while `reportHost` is the installer placeholder (the
  JSON field `reportReceiverConfigured` is then `false`);
- the pairing link and any pairing problem while unpaired;
- the Relay identifier, certificate expiry and any renewal problem;
- whether the DICOM and HL7 listeners are open;
- the Telrad host;
- whether report pickup is connected, and the last pickup problem;
- reports delivered, refused and failed since the service started;
- the ledger entry count and any ledger write problem; and
- active and refused connections per protocol.

The command fails if the service is not running. Nothing it shows is clinical
or secret.

`GET /readyz` on the status address returns `200` when the Relay is paired,
both listeners are open, and the report pickup connection is up or was up
within the last five minutes. Otherwise it returns `503`. `ready` in `status`
means the same thing; `degraded` means paired but not ready.

Loss of the pickup connection does not stop order or image forwarding. If
Telrad's DICOM or HL7 port is unreachable, the clinic connection that needed it
is closed and the others continue.

## DICOM behaviour

For each PACS connection Relay opens one TLS connection to Telrad's DICOM port
and copies bytes both ways until either side closes. Relay does not parse the
association: the PACS negotiates AE titles, presentation contexts and transfer
syntaxes with Telrad directly and receives Telrad's C-STORE status unchanged.
Relay does not spool, retry or deduplicate; retry belongs to the PACS.

Connections beyond `maxDicomConnections` are closed at accept. If Telrad cannot
be reached, the PACS connection is closed without a response, which the PACS
treats as an association failure. A connection with no traffic in either
direction for `idleTimeoutSeconds` is closed.

## HL7 order behaviour

For each RIS connection Relay opens one TLS connection to Telrad's HL7 port and
forwards MLLP frames both ways, byte for byte. Relay reads each frame
completely before forwarding it. It closes both connections without an answer
when:

- bytes arrive outside a frame;
- a frame exceeds `hl7MaxBytes`;
- a started frame is not finished within `hl7FrameSeconds`;
- Telrad does not acknowledge a forwarded message within `telradAckSeconds`;
  or
- the RIS sends nothing for `idleTimeoutSeconds`.

Telrad validates orders and composes every acknowledgement. Relay reads only
`MSH-10`, `ORC-1` and each `OBR-18` from the RIS's messages and `MSA-1` and
`MSA-2` from Telrad's replies, correlating by `MSA-2` = `MSH-10`.

When Telrad answers `AA` to a message whose `ORC-1` is `NW` or `XO`, Relay
appends that message's `OBR-18` values to `accessions.ledger` and syncs the file
before forwarding the `AA`. If the write fails, Relay closes both connections
instead of forwarding the `AA` and shows the problem in `status`. The RIS
resends, Telrad returns its stored acknowledgement, and Relay records the
accession then. Ledger entries are never removed, including on cancellation.

## Report return

Relay keeps one outbound TLS connection to Telrad's report port. It reconnects
with backoff from 1 to 60 seconds, and reconnects after a certificate renewal.
Telrad sends one report at a time and waits for Relay's acknowledgement before
sending the next. For each report:

- If the report has no `OBR-18`, or any `OBR-18` is not in the ledger, Relay
  answers `AR` with `MSA-3` `Accession not ordered through this Relay` and
  does not contact the RIS.
- Otherwise Relay connects to `reportHost`:`reportPort`, sends the report
  unchanged and returns the receiver's acknowledgement to Telrad byte for byte,
  whether `AA`, `AE` or `AR`.
- If the receiver cannot be reached or does not return a valid acknowledgement
  for this message within `receiverAckSeconds`, Relay answers `AE`. Telrad
  retries later.

A report Relay cannot parse, or one without `MSH-10`, is a protocol error: Relay
closes the pickup connection and reconnects.

Relay keeps no delivery record. If the receiver accepted a report but the
acknowledgement did not reach Telrad, Telrad sends the report again. The RIS
must tolerate a duplicate message with the same `MSH-10`.

If the ledger is lost, reports for earlier orders are refused until the RIS
resends those orders.

## Certificate renewal and re-pairing

The certificate lasts 90 days. From 30 days before expiry the service generates
a new key and requests a new certificate, authenticated with the current one.
On success it replaces key and certificate together. A failure is retried daily
and shown in `status` as a renewal problem. Keep the enrolment endpoint
reachable so renewal can succeed.

An expired certificate cannot be renewed; the Relay must be paired again. Stop
the service, delete `identity.json` from the data directory (keep
`accessions.ledger`), start the service and run `telrad` for a new link.
Telrad revokes a Relay by refusing its certificate; pairing again is also the
recovery from revocation.

## Upgrades

Rerun the installer for the version you want. To install a specific release,
use its tagged URL, for example:

```bash
curl -fsSL https://github.com/telrad-au/relay/releases/download/vX.Y.Z/install.sh | sudo sh
```

On Windows, run the release's `install.ps1` the same way from an Administrator
PowerShell. The installer keeps the configuration and data directory, so the
Relay stays paired and keeps its ledger. Relay does not check for or apply
updates itself.

## Shutdown

On stop, Relay closes its listeners, lets in-flight DICOM and HL7 connections
and any report delivery already under way finish for up to 90 seconds, then
exits. It never acknowledges a report it has not delivered.

## Removal

Stop and disable the service, then remove the installed files. The data
directory holds the private key and the ledger; delete it only when the Relay
is being retired. Ask Telrad to revoke the Relay so its certificate is no
longer accepted.

## Logs

Linux logs go to the journal:

```bash
journalctl --unit telrad-relay
```

Windows logs go to the Application event log under the source `TelradRelay`.

Logs contain no message bytes, DICOM UIDs, HL7 control IDs, accession numbers,
patient identifiers, pairing tokens or key material. They may contain
connection and report counts, error categories, the Relay identifier and
certificate expiry.
