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
  IPv4 address of that connection's TCP peer, for example `198.51.100.10`. Telrad
  resolves the address to a company, VPN connection and session when it accepts
  the request, and rejects addresses that do not resolve. AE titles, HL7 sender
  fields and patient identifiers never select a tenant.
- Connections from peers that are not IPv4 are closed before any DICOM or HL7
  processing. IPv4-mapped IPv6 peers on a dual-stack listener count as IPv4.
- Credential-renewal requests belong to the gateway itself and carry no peer
  header. A gateway opens no control session and never polls.
- HL7 orders go straight to the HL7 ingest endpoint. The gateway does not sign
  orders, register a signing key or send a referral envelope. Telrad validates
  the order and returns the application ACK as it does for a clinic Relay.
- Reports travel the other way. A clinic Relay is outbound-only, so it polls
  for reports. The gateway runs on Telrad's own host, so the platform calls it
  over a private path, authenticated with a bearer token, and the gateway
  returns the RIS acknowledgement in the response. The gateway never initiates
  report work and keeps no report state.

The gateway keeps no revocation state. When a VPN is revoked, its tunnel stops
delivering packets, and Telrad rejects anything still attributed to it.

A compromised gateway could claim any peer address and so act as any VPN
clinic. The gateway host is inside Telrad's trust boundary, so this adds no new
exposure. For the same reason, a report signing key held by the gateway would
not protect anything.

The delivery token is the only authority for report delivery. Keep it as secret
as the gateway credential, and let only the platform reach the delivery
listener's private interface.

## Report delivery

The platform delivers each report with one synchronous request to the gateway's
delivery listener:

```http
POST /deliveries HTTP/1.1
Authorization: Bearer <delivery token>
Content-Type: application/json

{
  "deliveryId": "opaque-delivery-id",
  "destination": {"host": "192.0.2.42", "port": 2575},
  "messageControlId": "MSH-10 of the payload",
  "payload": "MSH|^~\\&|...",
  "payloadSha256": "lowercase hex SHA-256 of the payload"
}
```

The gateway checks the request before it contacts the RIS:

- `destination.host` must be a canonical, specified IPv4 address and
  `destination.port` must be from 1 to 65535.
- The payload must be a single ORU^R01 message with no embedded MSH segment or
  MLLP framing bytes, its SHA-256 must equal `payloadSha256`, and its MSH-10
  must equal `messageControlId`.

A report that fails these checks returns `outcome: "failed"` with
`error: "invalid_report"`, and the RIS is not contacted. A valid report is sent
over MLLP with a 10-second connect timeout and a 20-second exchange deadline,
and the whole request is bounded to 30 seconds. The gateway does not check a
report permit.

| Status | Body | Meaning |
| --- | --- | --- |
| 200 | `{"deliveryId", "outcome", "ackCode"?, "ackPayload"?, "error"?}` | The delivery ran. `outcome` is `accepted` or `failed`. |
| 400 | `{"error": "invalid_request"}` | Wrong `Content-Type`, a body over 2 MiB, invalid JSON, unknown fields, or a missing or invalid field. The RIS is not contacted. |
| 401 | `{"error": "unauthorized"}` | Missing or wrong bearer token. |
| 404 | `{"error": "not_found"}` | Any other path. |
| 405 | `{"error": "method_not_allowed"}` | Any method other than `POST`. |
| 503 | `{"error": "busy"}` or `{"error": "shutting_down"}` | Over `maxConcurrentDeliveries`, or the gateway is stopping. `Retry-After: 1`. |

A 200 response uses the same vocabulary as a clinic Relay's report result:

- `accepted`, with `ackCode: "AA"` and the RIS ACK in `ackPayload`;
- `failed` with `error: "clinic_rejected"`, the RIS `ackCode` and its ACK in
  `ackPayload`;
- `failed` with `error: "invalid_report"`, `"network_timeout"` or
  `"network_error"`.

The platform owns retries. The RIS must handle a retransmission of the exact
message without duplicate effects. A 503 or a failed request can be retried.

The gateway logs one line per delivery with the delivery ID, destination,
outcome, error code, ACK code and duration. It never logs the payload or the
ACK text.

When the gateway stops, it refuses new delivery requests and lets in-flight RIS
exchanges finish within the normal shutdown drain.

## Configuration

Start from [`packaging/relay.gateway.example.json`](../packaging/relay.gateway.example.json).
Gateway mode:

- requires `relayId`, `credentialPath` and all four `/v1/relay/*` HTTPS URLs on
  one origin. The credential renewal endpoints are derived from `pairingUrl`,
  but the gateway never pairs, so `auth` and `enroll` are rejected.
- requires `deliveryListenAddress`, `deliveryPort` and `deliveryTokenPath`.
  `deliveryListenAddress` is the canonical IPv4 address of the private
  interface that the platform reaches, and it must not be `0.0.0.0`.
  `deliveryPort` must differ from `dicomPort` and `hl7Port`.
- accepts `maxConcurrentDeliveries`, which defaults to 16 and must be from 1
  through 256.
- accepts `deliveryTlsCertPath` and `deliveryTlsKeyPath` together. When both are
  set, the delivery listener serves TLS 1.2 or later with that PEM certificate
  and key. Otherwise it serves plain HTTP. Plain HTTP carries the bearer token
  and reports in clear text, so use it only on a private interface that only
  the platform can reach.
- rejects `retrieval`, `disableDicomListener`, `reportHost`, `reportPort`,
  `updateManifestUrl` and `updatePublicKey`. The report destination
  environment variables are rejected as well.
- accepts `maxConnectionsPerPeer`, which defaults to 64 and must not exceed
  `maxConnections`. When a peer reaches this limit, its further connections are
  closed. The process-wide and per-protocol limits still apply.

Clinic configurations reject `maxConnectionsPerPeer` and every delivery
setting.

Relative `credentialPath`, `deliveryTokenPath`, `deliveryTlsCertPath` and
`deliveryTlsKeyPath` values are resolved against the configuration file's
directory.

Provision the gateway credential as a version 2 credential file with mode
`0600` at `credentialPath`. The Relay renews it through the normal credential
lifecycle, because HTTPS ingest still uses it.

Provision the delivery token file with mode `0600` in a directory with mode
`0700`. It holds one token of 32 to 256 printable ASCII characters without
spaces, optionally followed by a newline. The platform holds the same token.
The gateway reads it at startup, so restart the gateway after changing it.

Native installers do not support gateway mode. Run it as a container and
replace the image to update it.

## Readiness

`telrad doctor` checks the configuration, the stored credential, the delivery
token and any delivery TLS certificate and key. `telrad ready`, which the
container health check runs, checks the stored credential, requires a fresh
`ready` runtime status and checks that the configured DICOM, HL7 and delivery
sockets are held. It tries to bind each socket and expects the address to be in use; it does not
dial the listeners. Readiness does not depend on the platform: a gateway has no
control session, and `telrad status` reports `control connected: false`.
