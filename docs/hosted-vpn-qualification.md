# Hosted VPN qualification mode

`telrad --config /path/to/relay.json run` starts one process with shared
DICOM and HL7 listeners. It selects a child connector only from the observed
canonical IPv4 TCP peer address. Each child uses its own existing Relay schema-5
configuration, protected credential directory, signing key, HTTPS client,
connection limits, credential lifecycle and report control loop. Unknown sources
are closed before a protocol handler runs. Stopping the process cancels all
children and closes their active native sockets.

The static manifest below is for disposable synthetic testing. It cannot
enforce timely VPN revocation and must not be used as a clinic service.

Managed mode obtains a full binding snapshot from the Telrad API using one
separate runtime management credential. The response carries a 60-second
process lease and shorter per-binding deadlines. Relay validates identities,
destinations and connection budgets before replacing its live source-IP map.
Expired or removed bindings close active native sockets. It stores separate
credential, signing-key and configuration files for each child, and attaches
the assigned binding ID, generation and runtime lease to every authenticated
HTTPS request. An unknown source never reaches a child handler. The backend
must independently check the same binding and VPN state at clinical acceptance.

The management credential is a single `thr_v1_` token in a mode-`0600` file.
Its endpoint must be HTTPS on a private management listener. The token can
bootstrap assigned connector credentials, so protect its file and the state
directory as secrets. Managed mode requires a dedicated process and cannot be
mixed with static `bindings`, a root connector or PACS retrieval.

```json
{
  "schemaVersion": 5,
  "listenAddress": "0.0.0.0",
  "dicomPort": 11112,
  "hl7Port": 2575,
  "reportHost": "127.0.0.1",
  "reportPort": 2576,
  "credentialPath": "/var/lib/telrad-relay/unused-credential.json",
  "maxConnections": 128,
  "maxDicomConnections": 96,
  "maxHl7Connections": 32,
  "hostedRuntime": {
    "managementUrl": "https://ingest.example.invalid:3443/internal/relay/hosted/configuration",
    "managementCredentialPath": "/var/lib/telrad-relay/management-token",
    "stateDirectory": "/var/lib/telrad-relay/state"
  }
}
```

The process exits if its first snapshot cannot be verified. Later refresh
failures retain only the previously leased bindings until their deadlines;
they never extend authority locally. A second process using the same runtime
credential and a different boot ID waits for the first process lease to expire.
Do not put clinical payloads or credentials in the manifest or process logs.

The manifest contains no credentials. Its paths point to separate child Relay
configurations and credential directories already provisioned for separate
connector identities. Their `reportHost` values must be canonical translated
IPv4 aliases reachable through the gateway; the ordinary Relay report sender
uses those addresses without a new dialer.

```json
{
  "schemaVersion": 5,
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
