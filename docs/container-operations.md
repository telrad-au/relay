# Container operations

The container runs the same Relay as the native services, as UID `10001`, with
a read-only root filesystem, no additional Linux capabilities and one named
volume mounted at `/var/lib/telrad-relay`. The container does not update its
own image. Start, stop and restart it through the container runtime.

Settings come from `TELRAD_RELAY_<FIELD>` environment variables, for example
`TELRAD_RELAY_REPORT_HOST` and `TELRAD_RELAY_REPORT_PORT`. The fields and
defaults are listed in [native operations](native-operations.md#configuration).
`TELRAD_RELAY_REPORT_HOST` is required.

## First pairing

Containers pair with a single-use pairing token from Telrad. Use the
`compose.yml` from the [README](../README.md#docker-compose), then:

```bash
read -rsp 'Pairing token: ' TELRAD_RELAY_PAIRING_TOKEN && printf '\n'
export TELRAD_RELAY_PAIRING_TOKEN
docker compose run --rm --env TELRAD_RELAY_PAIRING_TOKEN relay enroll
unset TELRAD_RELAY_PAIRING_TOKEN
docker compose up --detach
```

`enroll` generates the key, sends the certificate request with the token and
stores the issued identity in the volume. Telrad issues the certificate
immediately because the token already names the company. Relay removes the
token from its environment as soon as it reads it and never writes or logs it.
Running `enroll` on a paired volume does nothing. Do not put the token in a
Compose or environment file.

An unpaired container started without a token exits with an error saying that
`TELRAD_RELAY_PAIRING_TOKEN` is required.

Official images contain the production enrolment endpoint. Set
`TELRAD_RELAY_ENROLMENT_URL` only for development.

## Networking

Publish TCP `11112` only to DICOM sources and TCP `2575` only to HL7 sources.
Outbound, the container needs TCP to Telrad's DICOM, HL7 and report ports,
which pairing supplies, and HTTPS on TCP `443` to the enrolment endpoint.
Enrolment requests honour `HTTPS_PROXY` and `NO_PROXY`; the DICOM, HL7 and
report connections do not use a proxy.

The report receiver must be reachable from the container:

- use a routable address for another host;
- use a Compose service name for a receiver on the same Docker network;
- use `host.docker.internal` on Docker Desktop; or
- add `host.docker.internal:host-gateway` on Linux when that topology is
  explicitly chosen.

Relay verifies Telrad's certificates with the CA roots in the image. Do not add
a private CA to bypass certificate errors.

## Health

The status endpoint listens on loopback inside the container and is not
published. Check it with:

```bash
docker compose exec relay telrad-relay status
```

The command prints the state (`pairing`, `ready` or `degraded`), the report
receiver (`NOT CONFIGURED` if `TELRAD_RELAY_REPORT_HOST` is the placeholder
`report-receiver.invalid`), certificate expiry, listener and pickup state, report counts and the ledger entry count. It
exits non-zero when the service's status endpoint cannot be reached; it does
not exit non-zero for `degraded`.

The container health check uses `/readyz`, which is healthy when the Relay is
paired, both listeners are open, and the report pickup connection is up or was
up within the last five minutes. Loss of report pickup does not stop order or
image forwarding.

On stop, Relay closes its listeners and lets in-flight connections and any
report delivery already under way finish for up to 90 seconds. Keep
`stop_grace_period` above that. Relay does not replay an interrupted DICOM
association; the PACS resends.

## Certificate renewal

Renewal is automatic from 30 days before the 90-day certificate expires and is
retried daily after a failure. If the certificate expires, the Relay must be
paired again: stop the container, remove `identity.json` from the volume (keep
`accessions.ledger`), and repeat [first pairing](#first-pairing) with a new
token.

## Upgrade and rollback

Change the image to the new version tag, then recreate the service without a
pairing token:

```bash
docker compose pull
docker compose up --detach
docker compose ps
```

The volume keeps `identity.json` and `accessions.ledger`, so the Relay stays
paired and keeps its ledger. Pin an immutable version tag or digest in
production rather than `latest`. To roll back, set the previous version tag and
recreate the service the same way.

## Backup

Back up the `telrad-relay-data` volume. It contains:

- `identity.json`: the Relay's private key and certificate. Protect the backup
  as a secret. Restoring it on another host makes that host this Relay; run
  only one container per identity.
- `accessions.ledger`: the report authorisation record. If it is lost, reports
  for orders placed before the loss are refused until the RIS resends those
  orders.

A backup older than the current certificate may hold an expired certificate;
the Relay then has to be paired again, but the ledger remains useful.

## Removal

`docker compose down` keeps the named volume. Removing `telrad-relay-data`
destroys the Relay's identity and ledger, so treat it as a separate, approved
destructive step. Never use `docker compose down -v` as an ordinary
troubleshooting step. Ask Telrad to revoke a Relay that is being retired.
