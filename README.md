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

### Linux service

```bash
curl -fsSL https://github.com/telrad-au/relay/releases/latest/download/install.sh | sudo sh
telrad
```

### Windows service

Run PowerShell as Administrator:

```powershell
irm https://github.com/telrad-au/relay/releases/latest/download/install.ps1 | iex
telrad
```

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

Official images contain the source-controlled production pairing endpoint.
Development and self-hosted deployments can override it at runtime with
`TELRAD_RELAY_PAIRING_URL`. Relay derives the fixed control and ingest paths
from that one administrator-approved origin.

## Connect your PACS or RIS

Once Relay is running, point clinic systems to the Relay host's LAN address:

- DICOM: called AE title `TELRAD`, TCP port `11112`;
- HL7: MLLP, TCP port `2575`; and
- report return: TCP port `2576` at the configured report receiver.

Allow only the required clinic systems to reach these ports. Validate the
route with approved test traffic before sending clinical data.

## Check and manage Relay

```text
telrad status
telrad doctor
telrad update
```

`status` shows whether ingest, Telrad connectivity, and report return are
available. `doctor` checks the installation and configuration.

`telrad update` only checks for an update. It does not change the host. After
reviewing the release, an administrator can approve that exact version with:

```text
telrad update VERSION
```

Relay verifies the release, safely restarts, checks readiness, and rolls back
if the update fails.

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
