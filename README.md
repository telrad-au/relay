# Telrad Relay

Telrad Relay connects a clinic's PACS and RIS to Telrad. It receives DICOM and
HL7 on the clinic network and sends them securely to Telrad over outbound TCP
`443`; it does not require an inbound internet firewall rule.

Relay is an application-specific connector, not a VPN or general-purpose
network tunnel. It does not give Telrad routable access to the clinic network.

Relay runs as a Linux service, Windows service, or Linux container.

## Quickstart

Native installation is the simplest option. After installation, `telrad`
prints a link that an authorized clinic administrator uses to approve the
host.

### System requirements

**Tested size:** Linux x64 VM with 2 vCPUs, 512 MiB RAM and 8 GiB disk (AWS `t3a.nano`).
**Estimated capacity:** approximately **200 mixed studies/hour** at 100 Mbit/s with
four DICOM connections.

### Linux service

```bash
curl -fsSL https://github.com/telrad-au/relay/releases/latest/download/install.sh | sudo sh
telrad
```

### Windows service

Run the installer in PowerShell as Administrator:

```powershell
irm https://github.com/telrad-au/relay/releases/latest/download/install.ps1 | iex
```

Then run `telrad` from an ordinary terminal. It requests administrator authorization
for the specific management action; pairing runs inside the unprivileged service.

### Docker Compose

Save this as `compose.yml` and replace `192.0.2.20` with the report receiver's
address.

```yaml
services:
  relay:
    image: ghcr.io/telrad-au/relay:latest
    restart: unless-stopped
    # Prevent writes outside the persistent relay-data volume.
    read_only: true
    # Relay does not require any additional Linux capabilities.
    cap_drop:
      - ALL
    # Prevent the process from gaining additional privileges.
    security_opt:
      - no-new-privileges:true
    # Allow active DICOM and HL7 exchanges to finish during shutdown.
    stop_grace_period: 2m
    environment:
      # Hostname or IP address that receives returned reports.
      TELRAD_RELAY_REPORT_DESTINATION_HOST: "192.0.2.20"
      # TCP port used by the report destination.
      TELRAD_RELAY_REPORT_DESTINATION_PORT: "2576"
    ports:
      - "11112:11112/tcp"
      - "2575:2575/tcp"
    volumes:
      - relay-data:/var/lib/telrad-relay

volumes:
  relay-data:
    name: telrad-relay-data
```

Docker requires a one-time pairing token from Telrad.

#### Linux

```bash
read -rsp 'Pairing token: ' TELRAD_RELAY_PAIRING_TOKEN && \
  export TELRAD_RELAY_PAIRING_TOKEN && printf '\n'
docker compose run --rm --env TELRAD_RELAY_PAIRING_TOKEN relay enroll
unset TELRAD_RELAY_PAIRING_TOKEN
docker compose up --detach
```

#### Windows

Run in PowerShell:

```powershell
$SecureToken = Read-Host "Pairing token" -AsSecureString
$env:TELRAD_RELAY_PAIRING_TOKEN = [Net.NetworkCredential]::new("", $SecureToken).Password
docker compose run --rm --env TELRAD_RELAY_PAIRING_TOKEN relay enroll
Remove-Item Env:TELRAD_RELAY_PAIRING_TOKEN
docker compose up --detach
```

Docker retains configuration and authentication in the `telrad-relay-data`
volume after enrollment.

Pairing negotiates short-lived credentials when the Telrad service supports
them. Relay renews those credentials automatically before expiry and migrates
an existing bearer credential without requiring re-pairing. Keep the protected
credential volume persistent: it contains the crash-recovery state needed to
complete an interrupted renewal safely.

Official images contain the source-controlled production pairing endpoint.
Development and self-hosted deployments can override it at runtime with
`TELRAD_RELAY_PAIRING_URL`. Relay derives the fixed control and ingest paths
from that one administrator-approved origin.

## Connect your PACS or RIS

Relay supports two image transfer modes: **Push** and **Retrieve** (pull).
Push is the default. Choose the company mode in Telrad under
**Settings → Data exchange → Image transfer**. Retrieve also requires a
retrieval-capable Relay build, enabled cloud support and local PACS approval.

| Mode | Clinic connection | When images are transferred |
| --- | --- | --- |
| **Push** | PACS → Relay using DICOM C-STORE | When the PACS sends or routes images to Telrad. |
| **Retrieve (pull)** | Relay → PACS using C-FIND and C-GET | After an approved HL7 order authorizes retrieval and Telrad schedules the transfer. |

In both modes, Relay forwards images to Telrad over outbound HTTPS on TCP `443`.
The cloud does not connect directly to the PACS, and neither mode requires an
inbound internet firewall rule.

### Push mode

Add Relay as a DICOM destination in your PACS:

- **Host:** the Relay host's LAN address.
- **Port:** TCP `11112` by default.
- **Called AE title:** any valid AE title, for example `TELRAD`.

Allow the PACS to reach that local port, then configure manual sending or
automatic routing. C-ECHO checks connectivity; validate actual C-STORE transfer
with approved test images before sending clinical data. Push mode does not
query the PACS or fetch images in response to an order.

### Retrieve mode (pull)

Configure the PACS's query/retrieve host, port and called AE title in Relay,
with a clinic-approved calling AE title and the Storage SOP classes to receive.
Allow Relay to reach the PACS on that local port. The PACS must support Study Root
C-FIND and C-GET and accept Relay's Storage SCP role on the C-GET association.

An approved HL7 order supplies the accession. Relay searches the configured PACS
within the approved clinic/PACS namespace and retrieves every matching study.
Patient ID is not required for lookup or authorization. Later retrieval attempts
query again and can discover additional studies and images under that accession.

C-GET returns images over the connection Relay opened, using C-STORE
suboperations. No separate PACS-to-Relay callback or C-MOVE destination is needed.
Retrieve mode can also accept ordinary pushed images; keep Relay's DICOM listener
and its local firewall rule enabled if you need both.

The current retrieval profile supports native Explicit and Implicit VR Little
Endian images and ASCII accessions up to 16 bytes. See the
[retrieval provisioning and qualification guide](docs/pacs-retrieval.md) for local
signing-key/source approval, PACS configuration and activation checks. Selecting
Retrieve in Telrad alone does not configure or authorize access to the PACS.

### RIS orders and report return (both modes)

Point the RIS's HL7 sender to the Relay host's LAN address on MLLP TCP `2575`.
Configure Relay to deliver returned reports to the RIS/report receiver's LAN
address and listening port, TCP `2576` by default. The report receiver opens that
listener; Relay connects to it. Restrict each local port to the required clinic
systems and validate the routes with approved test traffic.

Report return requires a clinic-signed permit from an approved HL7 order for the
report's accession. Relay verifies that permit and the configured RIS destination
before delivery. This works in both modes; see [report authorization](docs/report-authorization.md).
Authorization is automatic and requires no additional configuration. This establishes order
authority; it does not authenticate the clinical report text.

Report return uses outbound HTTPS polling, with up to three seconds of idle
pickup latency. Telrad retains the delivery queue; Relay has no local delivery
ledger. If delivery succeeds but its confirmation is lost, the RIS may receive
the same report and message control ID again. The receiver must handle duplicates.

## Check and manage Relay

```text
telrad status
telrad doctor
telrad update
```

`status` shows whether ingest, Telrad connectivity, and report return are
available. `doctor` checks the installation and configuration. These commands,
`ready`, and update checks never request elevation or repair state. When the
service is stopped, credential diagnostics are unavailable.

A new native installation starts local management under its dedicated service
identity. DICOM and HL7 listeners remain closed until pairing succeeds. Pairing,
an immediate credential renewal, service control, and exact update application
require administrator authorization; the CLI announces each action before sudo
or UAC. Routine credential renewal is automatic and does not request elevation.

`telrad update` only checks for an update. It does not change the host. After
reviewing the release, an administrator can approve that exact version with:

```text
telrad update VERSION
```

Relay verifies the release, safely restarts, checks readiness, and rolls back
if the update fails.

## Why not a VPN?

Connecting a clinic to a reporting provider should be straightforward and give
the provider only the access needed to exchange studies, orders, and reports.
Relay is designed around that requirement.

- **Simpler setup.** Install Relay, approve the host through Telrad, and point
  your PACS or RIS to its local address. Relay connects over outbound HTTPS on
  TCP `443`, with no inbound internet firewall rules, public static IP, or
  site-to-site VPN configuration required. Local firewall rules and any
  outbound allowlisting still apply.

- **A narrower security boundary.** Relay handles specific DICOM and HL7
  exchanges without giving Telrad routable access to the clinic network. There
  is no general-purpose tunnel through which unrelated services can be
  reached. A VPN can be tightly restricted, but those restrictions must be
  configured and maintained separately.

- **Encrypted, authenticated communication.** Traffic between Relay and Telrad
  uses TLS with certificate verification and credentials specific to the
  enrolled Relay. Studies and orders travel outbound; returned reports are
  collected through outbound HTTPS requests and delivered to the configured
  local receiver.

- **Fewer networking dependencies.** There are no VPN peers, tunnel routes, or
  cross-site address translations to coordinate. Clinics can use overlapping
  private address ranges without conflicts between sites, and changing an
  internet connection does not require renegotiating a VPN peer configuration.

- **Fits existing clinical systems.** Your PACS and RIS continue using
  familiar DICOM and HL7 interfaces on the local network. Relay handles the
  connection to Telrad, so those systems do not need native support for cloud
  APIs or internet-facing endpoints.

- **Easier operation and review.** Built-in status and diagnostic commands
  distinguish clinical connectivity from Telrad connectivity and
  authentication problems. Relay is open source, does not persist clinical
  payloads, and applies verified software updates only when an administrator
  approves a specific version.

A well-configured VPN can provide secure connectivity. Relay’s advantage is a
smaller integration to configure, review, and maintain for this particular
workflow. It still requires a secured host and appropriate local network
controls, but it removes the need to operate a network tunnel just to exchange
clinical data.

## Security and privacy

Relay uses encrypted outbound connections and normal operating system
certificate verification. Credentials are stored with restricted permissions.

Relay does not persist clinical ingest payloads or returned report payloads,
or write clinical payloads or credentials to operational logs.

Relay is open-source under the Apache License 2.0, allowing clinics and their
security assessors to inspect its network, data-handling, and update behavior.
Signed release metadata identifies the corresponding source commit, and Relay
applies updates only after an administrator approves an exact verified version.

## Detailed documentation

- [Native service operations](docs/native-operations.md): firewall, proxy,
  health, authentication, updates, and recovery.
- [Container operations](docs/container-operations.md): networking, health,
  upgrades, and rollback.
- [Release documentation](docs/releases.md): release channels, downloads, and
  verification.
- [Configuration reference](packaging/relay.example.json): advanced listener,
  timeout, and report-return settings.

## Licence

Telrad Relay is open-source software licensed under the
[Apache License 2.0](LICENSE).

Copyright © 2026 Telrad Pty Ltd. Third-party terms and attributions are in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md), and project attribution is
in [`NOTICE`](NOTICE). The licence does not provide credentials or access to
Telrad-hosted services and does not grant trademark rights beyond its terms.
