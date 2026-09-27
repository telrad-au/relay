# Telrad Relay

Telrad Relay connects a clinic's PACS and RIS to Telrad when those systems can
only speak plain DICOM and plain HL7 over MLLP. It accepts connections on the
clinic network, wraps them in TLS with a client certificate that identifies the
clinic, and forwards them to Telrad. It makes outbound connections only; it
does not require an inbound internet firewall rule.

Relay is a TLS-wrapping TCP proxy plus a report gate. It does not change the
DICOM or HL7 bytes it forwards, and it is not a VPN or general-purpose tunnel.
It does not give Telrad routable access to the clinic network.

Relay runs as a Linux service, a Windows service, or a Linux container.

## Quickstart

Native installation is the simplest option. After installation, `telrad`
prints a link that an authorised person uses to approve the Relay.

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

Then run `telrad` from an ordinary terminal to see the pairing link.

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
      # Hostname or IP address of the RIS report receiver.
      TELRAD_RELAY_REPORT_HOST: "192.0.2.20"
      # TCP port of the RIS report receiver.
      TELRAD_RELAY_REPORT_PORT: "2576"
    ports:
      - "11112:11112/tcp"
      - "2575:2575/tcp"
    volumes:
      - relay-data:/var/lib/telrad-relay

volumes:
  relay-data:
    name: telrad-relay-data
```

A container pairs with a one-time pairing token from Telrad.

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

The `telrad-relay-data` volume keeps the Relay's key, certificate and accession
ledger. Keep it for the life of the installation.

## Pairing

Pairing gives the Relay a client certificate issued by Telrad. The certificate
names the Relay; Telrad records which company the Relay belongs to.

- **Native services** pair by link. The service asks Telrad for a pairing
  request and `telrad` prints the verification link. An authorised person opens
  it, signs in to Telrad, chooses the company and approves. The service then
  receives its certificate and Telrad's addresses and opens its listeners.
- **Containers** pair by token. `enroll` sends the token with the certificate
  request and Telrad issues the certificate immediately, because the token
  already names the company. The token is used once and never stored.

The DICOM and HL7 listeners stay closed until pairing succeeds. Relay renews its
certificate automatically; see [Security and privacy](#security-and-privacy).

## Connect your PACS and RIS

| Direction | Clinic system | Relay |
| --- | --- | --- |
| Images | PACS sends DICOM C-STORE | listens on TCP `11112` |
| Orders | RIS sends HL7 orders over MLLP | listens on TCP `2575` |
| Reports | RIS report receiver listens over MLLP | connects to `reportHost`:`reportPort` (default `2576`) |

Add Relay as a DICOM destination in the PACS, using the Relay host's LAN
address and port `11112`. Relay passes the association straight through to
Telrad, so AE titles, presentation contexts and the C-STORE status are
negotiated between the PACS and Telrad. Validate transfer with approved test
images before sending clinical data.

Point the RIS's HL7 order sender at the Relay host's LAN address on port
`2575`. Relay forwards each message and returns Telrad's acknowledgement
unchanged.

Configure `reportHost` and `reportPort` to the RIS's MLLP report receiver. The
receiver opens that listener; Relay connects to it for each report.

Restrict each local port to the clinic systems that need it.

## How report authorisation works

Relay delivers a report only for studies the clinic ordered through it. When
Telrad acknowledges a new order (`ORC-1` `NW` or `XO`) with `AA`, Relay records
each of the order's accession numbers (`OBR-18`) in a local ledger and writes
it to disk before passing the `AA` back to the RIS. A report whose accession
numbers are not all in the ledger is refused without contacting the RIS.
Relay keeps no delivery record, so if a delivery succeeds but Telrad does not
receive the acknowledgement, Telrad sends the report again. The RIS must accept
a duplicate message with the same control ID without duplicate clinical effect.

## Check and manage Relay

```text
telrad                  Show status, including the pairing link while unpaired
telrad status           Show status
telrad start            Start the service
telrad stop             Stop the service
telrad restart          Restart the service
telrad version          Print the installed version
```

`status` shows the state (`pairing`, `ready` or `degraded`), certificate
expiry, listener state, Telrad connectivity, report pickup, report counts and
the number of ledger entries. `telrad` never requests elevation; `start`,
`stop` and `restart` need the same rights as managing the service directly.

To update, rerun the installer for the version you want. Containers update by
pulling a new image. Relay does not update itself.

## Firewall

Relay needs no inbound internet rule. Outbound, it needs:

- TCP to Telrad's DICOM, HL7 and report ports. Pairing supplies the host and
  ports; `telrad status` shows the host.
- HTTPS on TCP `443` to Telrad's enrolment endpoint, for pairing and
  certificate renewal.

On the clinic network, allow the PACS to reach TCP `11112`, the RIS to reach
TCP `2575`, and Relay to reach the report receiver.

## Why not a VPN?

Relay is for clinics whose PACS and RIS cannot do TLS themselves and that do
not have a VPN to Telrad.

- If the clinic's systems support DICOM over TLS and MLLP over TLS with a
  client certificate, they can connect to Telrad directly without Relay.
- If the clinic has a site-to-site VPN to Telrad, its systems can send through
  the tunnel without Relay.
- Otherwise Relay provides the encryption and identity the clinic systems
  lack, without a tunnel to configure and maintain. It needs only outbound
  connections, gives Telrad no route into the clinic network, and handles only
  DICOM, HL7 orders and returned reports.

Relay works the same way inside a VPN; a tunnel changes only how Telrad's
addresses route. Relay still needs a secured host and appropriate local
network controls.

## Security and privacy

Relay holds one ECDSA P-256 private key, generated on the host and never sent
anywhere, and a client certificate issued by Telrad with a 90-day lifetime.
Relay presents the certificate on every DICOM, HL7 and report connection to
Telrad. From 30 days
before expiry it renews automatically with a new key. Relay verifies Telrad's
servers with the operating system's trust store and does not follow redirects.
The key and certificate are stored with permissions restricted to the service
account.

Relay does not persist clinical payloads. The ledger holds accession numbers
only. Logs contain no message bytes, DICOM UIDs, HL7 control IDs, accession
numbers, patient identifiers or key material.

Relay is open-source under the Apache License 2.0, so clinics and their
security assessors can inspect its network and data-handling behaviour.

## Detailed documentation

- [Native service operations](docs/native-operations.md): configuration,
  pairing, status, renewal, upgrades and removal.
- [Container operations](docs/container-operations.md): pairing, networking,
  health, upgrades and backup.
- [Architecture](docs/architecture.md): the design and the protocols Relay
  speaks to Telrad.
- [Release documentation](docs/releases.md): downloads and verification.

## Licence

Telrad Relay is open-source software licensed under the
[Apache License 2.0](LICENSE).

Copyright © 2026 Telrad Pty Ltd. Third-party terms and attributions are in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md), and project attribution is
in [`NOTICE`](NOTICE). The licence does not provide credentials or access to
Telrad-hosted services and does not grant trademark rights beyond its terms.
