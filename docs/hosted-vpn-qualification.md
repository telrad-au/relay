# Hosted VPN qualification mode

`telrad --config /path/to/hosted.json hosted-run` starts one process with shared
DICOM and HL7 listeners. It selects a child connector only from the observed
canonical IPv4 TCP peer address. Each child uses its own existing Relay schema-5
configuration, protected credential directory, signing key, HTTPS client,
connection limits, credential lifecycle and report control loop. Unknown sources
are closed before a protocol handler runs. Stopping the process cancels all
children and closes their active native sockets.

This static mode is for disposable synthetic AWS qualification of issue 95. The
application-managed configuration lease, binding generations, VPN state checks,
backend acceptance gate, and hosted report-route persistence are not implemented
here. Static configuration cannot enforce timely VPN revocation and must not be
used as a production clinic service. Only deploy it behind the trusted VPN
gateway with source identity preserved and ingress restricted to that gateway.

The manifest contains no credentials. Its paths point to separate child Relay
configurations and credential directories already provisioned for separate
connector identities. Their `reportHost` values must be canonical translated
IPv4 aliases reachable through the gateway; the ordinary Relay report sender
uses those addresses without a new dialer.

```json
{
  "schemaVersion": 1,
  "listenAddress": "100.96.0.1",
  "dicomPort": 11112,
  "hl7Port": 2575,
  "maxConnections": 32,
  "maxDicomConnections": 24,
  "maxHl7Connections": 8,
  "bindings": [
    { "sourceIp": "100.96.0.10", "configPath": "bindings/a/relay.json" },
    { "sourceIp": "100.96.0.11", "configPath": "bindings/b/relay.json" }
  ]
}
```

The gateway must preserve the source identity through to the TCP listener. The
child's credential must authenticate to the matching company and connector; a
synthetic two-clinic test must prove that clinic A cannot cause traffic under
clinic B's credential or receive clinic B's report. The static mode deliberately
does not create or modify application records or VPN translations.
