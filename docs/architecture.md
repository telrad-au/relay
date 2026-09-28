# Relay architecture

Status: accepted design, 2026-09-27. This document supersedes the HTTPS ingest,
credential lifecycle, report permit, PACS retrieval, gateway mode and self-update
designs described elsewhere in this repository. Those documents are retained
until their code is removed and are marked as superseded.

## Requirements

Relay exists so that a clinic whose PACS and RIS speak only plain DICOM and
plain HL7 v2 over MLLP can exchange studies, orders and reports with Telrad.
It has six requirements and no others:

1. Authorise reports: deliver a report to the RIS only when the clinic ordered
   the study through this Relay.
2. Encrypt outbound data.
3. Associate outbound data with a company.
4. Send data to Telrad in a standard protocol.
5. Do not manipulate data. Framing changes; bytes do not.
6. Run inside a VPN without needing the VPN.

Clinics whose systems support TLS natively connect to the same Telrad listeners
directly and do not run Relay. Clinics on a site-to-site VPN may connect their
plain systems directly through the tunnel. Relay is for everyone else.

## Shape

Relay is a TCP proxy that wraps TLS, plus a report gate. It has one mode.

```text
PACS ──C-STORE──▶ :11112 ─┐                 ┌─▶ Telrad DICOM listener (DICOM over TLS)
                          │  Relay          │
RIS ──ORM/MLLP──▶ :2575 ──┤  client cert ───┼─▶ Telrad HL7 listener   (MLLP over TLS)
                          │                 │
RIS ◀──ORU/MLLP── gate ◀──┘◀── pickup ◀─────┴── Telrad report port     (MLLP over TLS)
```

Every connection Relay makes is outbound: TCP to Telrad's DICOM, HL7 and report
ports, and HTTPS to Telrad's enrolment endpoint during pairing and renewal. The
clinic firewall needs no inbound rule. Relay runs identically over the public
internet and inside a VPN; a tunnel only changes how Telrad's addresses route.

Relay holds one client certificate. That certificate is what associates traffic
with a company and it is presented on every connection to Telrad's DICOM, HL7
and report ports, including over a VPN, so Telrad always knows the traffic came
through a Relay.

## DICOM

Relay listens on `dicomPort` (default 11112). For each accepted clinic
connection it opens one TLS connection to Telrad's DICOM listener and copies
bytes in both directions until either side closes. Relay does not parse the
association, PDUs or DIMSE messages. The PACS negotiates presentation contexts,
transfer syntaxes and AE titles with Telrad's SCP directly, and receives
Telrad's C-STORE status unchanged. Retry is the PACS's, as it is for any
DICOM destination.

The connection limit `maxDicomConnections` bounds concurrent associations;
excess clinic connections are refused at accept. When the upstream connection
cannot be established the clinic connection is closed without a response, which
DICOM senders treat as association failure.

## HL7 orders

Relay listens on `hl7Port` (default 2575). For each accepted clinic connection
it opens one TLS connection to Telrad's HL7 listener and forwards MLLP frames in
both directions. Frames are `0x0B message 0x1C 0x0D`. Relay reads each frame
completely before forwarding it, bounded by `hl7MaxBytes` (default 1 MiB) and
`hl7FrameSeconds` (default 30). Bytes outside a frame, an oversize frame or an
incomplete frame close both connections without an answer.

Relay forwards every frame byte for byte. It reads three fields from each
clinic message: MSH-10, ORC-1 and every OBR-18. It reads MSA-1 and MSA-2 from
each Telrad reply. It correlates reply to message by MSA-2 = MSH-10, not by
order of arrival. A reply whose MSA-2 matches no message on that connection is
forwarded anyway.

### Ledger

When a Telrad reply is `AA` and the correlated message has ORC-1 `NW` or `XO`,
Relay appends each of that message's OBR-18 values to the ledger and syncs the
file to disk **before** forwarding the reply to the RIS. If the append fails,
Relay closes both connections instead of forwarding the `AA`; the RIS resends,
Telrad returns its stored acknowledgement, and Relay records the accession then.
This ordering means the RIS never holds an `AA` for an order that the ledger
does not know.

The ledger is the only clinical state Relay keeps. Beyond it Relay keeps only
its configuration, its identity and, while one is open, a
[backlog acceptance](#backlog-acceptance) window. The ledger is:

- one file, `accessions.ledger`, on the data volume;
- one accession per line: the OBR-18 string with surrounding whitespace
  removed and no other normalisation;
- append-only, synced per write, loaded into memory at start;
- entries are never removed, including on `CA` or `DC`, and there is no
  retention limit. At the tested rate of 200 studies per hour the file grows by
  roughly 30 MiB a year;
- TEST-lane messages (MSH-11 `T`) are recorded like any other.

A message with no OBR-18 creates no entry. Telrad rejects such orders, so this
is not a gap. If the ledger is lost, reports for earlier orders are refused
until the RIS resends those orders or the clinic opens a backlog acceptance
window; see [Recovery after losing the Relay host](#recovery-after-losing-the-relay-host).

Accession values appear in the ledger file and nowhere else. They are never
logged.

## Report pickup

Relay holds one outbound TLS connection to Telrad's report port, reconnecting
with exponential backoff (1 s to 60 s) whenever it closes. The connection
carries MLLP frames with the roles of the classical MLLP exchange preserved and
only the dialer reversed: Telrad writes one framed ORU, Relay answers one framed
acknowledgement, and Telrad does not write the next report until it has that
acknowledgement. Relay treats any other interleaving as a protocol error and
closes the connection.

For each report Relay:

1. Reads every OBR-18 in the ORU. If the ORU has none, or any value is absent
   from the ledger and no backlog acceptance window is open, Relay answers `AR`
   with MSA-3 `Accession not ordered through this Relay` and does not contact
   the RIS. While a window is open, Relay appends the absent values to the
   ledger and syncs it, exactly as for an accepted order, before continuing.
   If that append fails, Relay closes the pickup connection without an
   acknowledgement and Telrad retries.
2. Otherwise connects to the clinic report receiver at `reportHost`:`reportPort`
   (default 2576), writes the ORU frame unchanged, and reads one framed reply.
3. Returns the receiver's reply to Telrad byte for byte, whether `AA`, `AE` or
   `AR`.
4. If the receiver cannot be reached, closes without a reply, or replies with
   something that is not one acknowledgement frame answering this report,
   Relay answers Telrad with `AE` and MSA-3 `Report receiver unavailable`.
   Telrad retries later.

Acknowledgements Relay composes itself carry the ORU's MSH-10 in MSA-2, its
sending and receiving applications swapped, MSH-11 copied, and a fresh MSH-10.
Relay keeps no delivery record. If the receiver accepted a report but the
connection to Telrad dropped before the acknowledgement arrived, Telrad sends
the report again. The RIS must tolerate a duplicate message with the same
control ID, as it must for any HL7 integration.

Report pickup does not start until Relay holds a certificate. Loss of the
pickup connection does not affect order or image forwarding.

### Backlog acceptance

The ledger authorises reports for orders placed through this Relay. After the
ledger is lost, for example when a Relay host is replaced, Telrad still holds
reports for orders the new ledger has never seen. The clinic, not Telrad,
decides to let those through:

```text
telrad accept-backlog [--hours N]     # N from 1 to 168, default 72
telrad accept-backlog --cancel
```

The command writes `accept-backlog.json`, `{ "until": "<RFC3339>" }`, to the
data volume with permissions restricted to the service account, and prints the
window. The running service reads the file for every report, so no restart is
needed. While the current time is before `until`, a report whose OBR-18 values
are not all in the ledger is accepted: each absent value is appended to the
ledger and synced before the report is forwarded, and Relay logs one line per
recorded accession without its value. A report with no OBR-18 is still
refused. After `until` the gate is normal again and the file is deleted the
next time Relay finds it expired; `--cancel` deletes it at once. The window is
shown in `status` and has no environment override: it is a deliberate
operator action on one host, not configuration.

## Identity and pairing

At first start Relay generates an ECDSA P-256 private key on the data volume
with permissions restricted to the service account and a certificate signing
request whose subject is empty. Telrad assigns the identity; Relay does not
claim one. Telrad issues the certificate from its own private certificate
authority with the opaque Relay identifier as subject and a 90-day lifetime.
Company membership lives in Telrad's database and is never read from the
certificate.

Relay trusts two things, for two different connections. The enrolment endpoint
is a public HTTPS service, verified with the operating system's trust store.
Telrad's DICOM, HL7 and report ports present certificates from the Telrad Relay
CA, and Relay verifies them against that CA only, never the operating system's
roots. The CA certificate arrives in the issued identity (`telrad.caCertificate`)
over the OS-verified enrolment connection and is stored in `identity.json`; a
renewal may replace it, which is how Telrad rotates the CA. Relay ships no
trust material and never follows redirects. An `identity.json` without a CA
certificate is treated as unpaired and Relay pairs again.

### Interactive pairing (Linux and Windows services)

The service posts the CSR to the enrolment endpoint and receives a verification
link. `telrad` prints that link. An authorised person opens it, signs in to
Telrad, chooses the company and approves. The service polls until approval and
stores the certificate, Telrad's listener addresses and ports, and its Relay
identifier. Listeners open and report pickup begins.

### Token pairing (containers)

`docker compose run --rm relay enroll` posts the CSR together with a single-use
pairing token from `TELRAD_RELAY_PAIRING_TOKEN`. Telrad answers with the
certificate immediately because the token already names the company.

### Renewal

From 30 days before expiry Relay generates a new key and CSR and posts it to
the renewal endpoint over plain HTTPS, with no client certificate. The request
carries the current leaf certificate and a signature by the current private key
over the new CSR and a timestamp, so Telrad can check that the holder of the
current key asked for the new certificate. On success Relay swaps key and
certificate atomically. Every new upstream connection presents
the new certificate, and the report pickup connection is re-established at
once; DICOM and HL7 connections already open finish on the old certificate. A
renewal failure is retried daily and shown in `status`. An expired certificate
cannot renew. A relay that starts with an expired certificate reports the
expiry in `status` and returns to pairing exactly as on a fresh install (the
link flow on a native install, a token in a container). A certificate that
expires while the service is running is reported in `status`; restarting the
service begins pairing. Telrad revokes a Relay by refusing its
certificate; short lifetimes bound the exposure of a stolen key.

### Enrolment endpoint contract

Relay carries one URL, `enrolmentUrl`. Official builds contain the production
value, `https://app.telrad.com.au/api/relay/enrolments`, on Telrad's app host;
`TELRAD_RELAY_ENROLMENT_URL` overrides it for development. The path carries no
version; the protocol version is the `X-Telrad-Relay-Protocol` header. Requests
carry `X-Telrad-Relay-Protocol: 2` and JSON bodies. Responses are `Cache-Control:
no-store`.

| Request | Response |
| --- | --- |
| `POST {enrolmentUrl}` `{ csr, agentVersion, platform, hostname }` | `201 { enrolmentId, verificationUrl, pollSeconds, expiresAt }` |
| `POST {enrolmentUrl}` `{ csr, agentVersion, platform, hostname, pairingToken }` | `200` issued (below), or `403` |
| `GET {enrolmentUrl}/{enrolmentId}` | `202` pending, `200` issued, `410` expired or denied |
| `POST {enrolmentUrl}/renew` `{ csr, agentVersion, certificate, signedAt, signature }` | `200` issued, or `403` |

`platform` is `linux` or `windows` for a native service and `docker` for the
container. `hostname` is 1 to 253 characters and `agentVersion` 1 to 64.

Renewal is a plain HTTPS request without a client certificate:

```json
{
  "csr": "-----BEGIN CERTIFICATE REQUEST-----…",
  "agentVersion": "1.2.3",
  "certificate": "-----BEGIN CERTIFICATE-----…",
  "signedAt": "2026-11-26T00:00:00Z",
  "signature": "base64"
}
```

`csr` is the request for the new key. `certificate` is the current leaf only.
`signedAt` is the signing time in RFC 3339 UTC. `signature` is the base64 of the
raw ECDSA P-256 signature, `r || s` as 64 bytes, made with the current private
key over SHA-256 of the CSR PEM bytes, a line feed, and the `signedAt` bytes.
Telrad answers `403` unless the certificate chains to the Relay CA, is
unexpired and unrevoked, the signature verifies with its key, and `signedAt` is
recent.

Issued:

```json
{
  "relayId": "opaque",
  "certificate": "-----BEGIN CERTIFICATE-----…",
  "notAfter": "2026-12-26T00:00:00Z",
  "telrad": {
    "host": "app.telrad.com.au",
    "dicomPort": 2762,
    "hl7Port": 2576,
    "reportPort": 2578,
    "caCertificate": "-----BEGIN CERTIFICATE-----…"
  }
}
```

`certificate` may contain the issuing chain after the leaf. `caCertificate` is
one or more PEM CA certificates, the Telrad Relay CA that Relay pins for the
data ports; Relay rejects an issued identity whose `caCertificate` is not at
least one CA certificate. It is required when pairing. A renewal response may
omit it to keep the stored CA or carry a new one to replace it. Relay stores
`telrad` and uses it for every connection to the data ports; a renewal may
change it.

## Status

The service listens on `statusAddress` (default `127.0.0.1:8425`, loopback
only) and answers:

- `GET /readyz`: `200` when paired, both listeners bound and the report pickup
  connection established within the last five minutes; otherwise `503`. This is
  the container health check.
- `GET /status`: JSON with pairing state, the verification link while unpaired,
  certificate expiry, listener state, time of the last successful connection to
  each Telrad port, report pickup state, the ledger entry count, and the
  configured report receiver with a flag that is false while it is still the
  installer's placeholder `report-receiver.invalid`, and `acceptBacklogUntil`,
  the end of an open backlog acceptance window or `null`.

`telrad status` reads `/status` and prints it. `telrad ready` reads `/readyz`
and exits non-zero when the relay is not ready; it is the container health
check. No other local management channel exists. Nothing on the status
endpoint is clinical or secret.

## Configuration

`relay.json`, schema version 6. Environment variables named
`TELRAD_RELAY_<FIELD>` override each field for containers. There is no upgrade
path from earlier schemas: Relay has not been released.

| Field | Default | Meaning |
| --- | --- | --- |
| `enrolmentUrl` | built in | Telrad enrolment endpoint |
| `dataDir` | platform default | key, certificate, ledger, state |
| `listenAddress` | `0.0.0.0` | clinic-facing bind address |
| `dicomPort` | `11112` | clinic DICOM listener |
| `hl7Port` | `2575` | clinic HL7 listener |
| `reportHost` | required | clinic report receiver; the installer prompts for it on a terminal, accepts `TELRAD_RELAY_REPORT_HOST`, and otherwise writes the placeholder `report-receiver.invalid`, which `status` flags |
| `reportPort` | `2576` | clinic report receiver port |
| `statusAddress` | `127.0.0.1:8425` | local status endpoint |
| `maxDicomConnections` | `128` | concurrent associations |
| `maxHl7Connections` | `128` | concurrent order connections |
| `hl7MaxBytes` | `1048576` | frame size bound |
| `hl7FrameSeconds` | `30` | deadline to complete a started frame |
| `connectTimeoutSeconds` | `10` | upstream and receiver dial timeout |
| `telradAckSeconds` | `60` | wait for Telrad's acknowledgement of a forwarded order |
| `receiverAckSeconds` | `30` | wait for the report receiver's acknowledgement |
| `idleTimeoutSeconds` | `900` | close a clinic connection idle this long |

Telrad's host and ports are not configuration. They are state written by
enrolment.

## Installation

Three targets, one binary. Updating is rerunning the installer with the wanted
version; Relay does not update itself.

- **Linux service.** `install.sh` downloads the release, verifies its checksum,
  creates the `telrad-relay` user, installs the systemd unit and starts it.
  `telrad` then prints the pairing link.
- **Windows service.** `install.ps1` does the same with a Windows service under
  a virtual service account.
- **Container.** `packaging/compose.yml` with a persistent data volume; pairing
  by token as above.

The service process runs unprivileged. `telrad` never requests elevation; the
installer is the only privileged step.

## Recovery after losing the Relay host

A lost or rebuilt Relay host is replaced, not restored:

1. If a copy of `accessions.ledger` exists, put it in the new data directory
   before starting the service. It holds no key material, so it can be backed
   up and restored like other clinic data. Never restore `identity.json`.
2. Install Relay on the replacement host and pair it (link or token). It
   receives a new identity.
3. A company administrator chooses **Replace** on the old Relay in Telrad's
   settings. Telrad revokes the old Relay and moves every outstanding and
   failed report delivery onto the replacement, which it then retries.
4. On the new host run `telrad accept-backlog` (72 hours by default). While
   the window is open Relay delivers the moved reports and records their
   accessions, so later corrected or addended reports pass the normal gate.
5. The window closes by itself; run `telrad accept-backlog --cancel` once the
   backlog is delivered to close it early.

Telrad retries a report Relay answers with `AR` or `AE` on its normal backoff,
about eight attempts over two days, so a report refused before the window
opened is delivered on a later attempt.

`identity.json` is not a backup item. It is the Relay's private key: a restored
copy on a second host makes two hosts one Relay, and a stale copy may hold an
expired certificate. Pair the replacement host instead.

## Shutdown

On `SIGTERM` Relay stops accepting, lets in-flight DICOM associations and HL7
exchanges complete for up to the stop grace period, finishes any report whose
receiver exchange has started, then exits. It does not acknowledge a report it
has not delivered.

## Privacy and logging

Relay does not persist clinical payloads. Logs contain no message bytes, DICOM
UIDs, HL7 control IDs, accession numbers, patient identifiers or key material.
They may contain connection counts, byte counts, durations, acknowledgement
codes, error categories and certificate expiry.

## What Telrad must provide

For the platform, in its own repository:

- MLLP over TLS and DICOM over TLS listeners requiring a client certificate from
  the Relay CA, resolving certificate to Relay to company at accept time,
  alongside the existing tunnel-address resolution for VPN clinics.
- A report port speaking the pickup protocol above: hold each report for a
  company whose orders arrived through a Relay, deliver on that company's live
  pickup connection, and retry a report answered `AE` or `AR` on its normal
  backoff (about eight attempts over two days).
- The enrolment endpoint, verification page, pairing tokens, the private CA and
  revocation. Issued identities carry the Telrad Relay CA in
  `telrad.caCertificate`, and the renewal endpoint verifies the signed request
  described above instead of a client certificate.
- A **Replace** action in Telrad settings for a Relay: it revokes the old
  Relay and moves every outstanding and failed delivery onto the replacement
  Relay, then retries them.

## Removed

Compared with the previous architecture Relay no longer has: HTTPS ingest,
bearer credentials and their renewal state machine, order signing keys and
report permits, a DICOM SCP, PACS retrieval, gateway mode, a report listener,
self-update and release trust roots, a privileged local IPC broker, `doctor`,
configuration schema upgrades, or the performance tooling under
`tools/relay-perf`.

## Tests

- Proxy: bytes in equal bytes out for DICOM and HL7 against a loopback
  mutual-TLS listener standing in for Telrad; connection limits; upstream
  failure and unauthenticated-client behaviour. Relay never interprets DICOM,
  so arbitrary bytes are a sufficient fixture.
- Ledger: AA/AE/AR correlation, NW/XO/CA filtering, multi-OBR orders, fsync
  before forward, append failure closes the connection, reload at start.
- Report pickup: authorised delivery with receiver AA/AE/AR echoed byte for
  byte, refusal without receiver contact, receiver unreachable, malformed
  receiver reply, reconnect and backoff, duplicate delivery, backlog acceptance
  recording absent accessions before delivery and ending with its window.
- Pairing: interactive and token flows against a loopback enrolment server,
  renewal swap, signed renewal and refusal of a tampered request, CA
  certificate validation, pinning and rotation, expired certificate returning
  to pairing, redirect rejection, file permissions.
- Installers: the existing bundle contract tests, reduced to the three targets.
