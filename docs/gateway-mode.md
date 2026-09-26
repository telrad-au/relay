# Gateway mode

Status: in development. Gateway mode is not in any release, and its platform
contract may still change.

Gateway mode runs one Relay process on Telrad's IPsec gateway host. That process
serves every clinic that connects over a site-to-site VPN. A clinic Relay
(`"mode": "clinic"`, or no `mode`) is unchanged.

## Trust model

The VPN tunnel vouches for the clinic. The gateway preserves each tunnel's
ingress identity address, so the TCP peer address on the Relay's listeners
identifies the tunnel.

The gateway Relay forwards traffic and reports that address. It does not decide
which clinic a connection belongs to:

- Every HTTPS ingest request that the Relay makes for a DICOM or HL7 connection
  carries exactly one `X-Telrad-Peer-Ip` header. Its value is the canonical
  IPv4 address of that connection's TCP peer, for example `100.96.0.10`. Telrad
  resolves the address to a company, VPN connection and session when it accepts
  the request, and rejects addresses that do not resolve. AE titles, HL7 sender
  fields and patient identifiers never select a tenant.
- Connections from peers that are not IPv4 are closed before any DICOM or HL7
  processing. IPv4-mapped IPv6 peers on a dual-stack listener count as IPv4.
- Control, poll, report-result and credential-renewal requests belong to the
  gateway itself and carry no peer header.
- HL7 orders go straight to the HL7 ingest endpoint. The gateway does not sign
  orders, register a signing key or send a referral envelope. Telrad validates
  the order and returns the application ACK as it does for a clinic Relay.
- Each report claim names its destination:
  `"destination": {"host": "100.100.0.42", "port": 2575}`. The host must be a
  canonical IPv4 address and the port must be from 1 to 65535. The gateway does
  not check a report permit. It still verifies the payload digest and MSH-10,
  and it accepts only a single ORU^R01 message with no embedded MSH. A claim
  without a valid destination fails with `invalid_report` and the RIS is not
  contacted.
- The control `hello` includes the capability `"gateway": true`.

The gateway keeps no revocation state. When a VPN is revoked, its tunnel stops
delivering packets, and Telrad rejects anything still attributed to it.

A compromised gateway could claim any peer address and so act as any VPN
clinic. The gateway host is inside Telrad's trust boundary, so this adds no new
exposure. For the same reason, a report signing key held by the gateway would
not protect anything.

## Configuration

Start from [`packaging/relay.gateway.example.json`](../packaging/relay.gateway.example.json).
Gateway mode:

- requires `relayId`, `credentialPath` and all four `/v1/relay/*` HTTPS URLs on
  one origin. The credential renewal endpoints are derived from `pairingUrl`,
  but the gateway never pairs, so `auth` and `enroll` are rejected.
- rejects `retrieval`, `disableDicomListener`, `reportHost`, `reportPort`,
  `updateManifestUrl` and `updatePublicKey`. The report destination
  environment variables are rejected as well.
- accepts `maxConnectionsPerPeer`, which defaults to 64 and must not exceed
  `maxConnections`. When a peer reaches this limit, its further connections are
  closed. The process-wide and per-protocol limits still apply. Clinic
  configurations reject `maxConnectionsPerPeer`.

Provision the gateway credential as a version 2 credential file with mode
`0600` at `credentialPath`. The Relay renews it through the normal credential
lifecycle. Native installers do not support gateway mode. Run it as a container
on the host network, and replace the container image to update it.

## Readiness

`telrad doctor` checks the configuration and the stored credential. `telrad
ready`, which the container health check runs, also requires a fresh `ready`
runtime status and checks that the configured DICOM and HL7 sockets are held.
It tries to bind each socket and expects `EADDRINUSE`. It does not dial the
listeners, because the VPN ingress firewall rejects loopback connections.
