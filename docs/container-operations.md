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
docker compose run --rm --env TELRAD_RELAY_PAIRING_TOKEN relay pair
unset TELRAD_RELAY_PAIRING_TOKEN
docker compose up --detach
```

`pair` generates the key, sends the certificate request with the token and
stores the issued identity in the volume. Telrad issues the certificate
immediately because the token already names the company. Relay removes the
token from its environment as soon as it reads it and never writes or logs it.
On a paired volume `pair` refuses and changes nothing unless given `--yes`; see
[pairing again](#pairing-again). Do not put the token in a Compose or
environment file.

An unpaired container started without a token exits with an error saying that
`TELRAD_RELAY_PAIRING_TOKEN` is required.

Official images contain the production enrolment endpoint. Set
`TELRAD_RELAY_ENROLMENT_URL` only for development.

## Pairing again

To pair a paired volume again, for example to move the Relay to another
company, stop the container and pair with a new token and `--yes`:

```bash
docker compose stop relay
read -rsp 'Pairing token: ' TELRAD_RELAY_PAIRING_TOKEN && printf '\n'
export TELRAD_RELAY_PAIRING_TOKEN
docker compose run --rm --env TELRAD_RELAY_PAIRING_TOKEN relay pair --yes
unset TELRAD_RELAY_PAIRING_TOKEN
docker compose up --detach
```

The stored identity is replaced only after Telrad has issued the new one, so a
refused or mistyped token leaves the current pairing in place. The ledger is
kept. Telrad keeps the old Relay until a company administrator revokes or
replaces it in settings.

The native `telrad report-receiver HOST` and `telrad uninstall` commands do
not apply to containers: change `TELRAD_RELAY_REPORT_HOST` and
`TELRAD_RELAY_REPORT_PORT` and recreate the container, and remove the
container as described under [removal](#removal).

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

Relay verifies the enrolment endpoint with the CA roots in the image, and
Telrad's DICOM, HL7 and report ports against the Telrad Relay CA only, which it
receives when pairing and keeps in the volume. Do not add a private CA to the
image to bypass certificate errors.

## Health

The status endpoint listens on loopback inside the container and is not
published. Check it with:

```bash
docker compose exec relay telrad-relay status
```

The command prints the state (`pairing`, `ready` or `degraded`), the report
receiver (`NOT CONFIGURED` if `TELRAD_RELAY_REPORT_HOST` is the placeholder
`report-receiver.invalid`), the end of an open backlog acceptance window,
certificate expiry, listener and pickup state, report counts and the ledger entry count. It
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

## Backlog acceptance

To let Telrad deliver reports for orders the ledger does not know, for example
after the Relay host is replaced, open a backlog acceptance window against the
same volume while the service runs:

```bash
docker compose run --rm relay accept-backlog --hours 72
```

`--hours` is 1 to 168 and defaults to 72. The command writes
`accept-backlog.json` to the volume and prints when the window ends; the
running container picks it up with the next report, without a restart. Until
then a report whose accession numbers are not all in the ledger is delivered,
and each missing accession number is appended to `accessions.ledger` and synced
before the report is sent to the RIS. `status` shows the window while it is
open. It closes by itself; close it early with:

```bash
docker compose run --rm relay accept-backlog --cancel
```

There is no environment variable for this: opening a window is a deliberate
operator action. While it is open Relay accepts every report Telrad sends for
the company, so open it only when a backlog is expected.

## Certificate renewal

Renewal is automatic from 30 days before the 90-day certificate expires and is
retried daily after a failure. Renewal requests are signed with the current key
and may also replace the Telrad Relay CA certificate. A volume whose
`identity.json` predates the pinned CA is treated as unpaired; pair it with a
new token. If the certificate expires, the Relay must be paired again: stop
the container and repeat [first pairing](#first-pairing) with a new token. An
expired pairing is replaced without `--yes`, and the ledger is kept.

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

Back up `accessions.ledger` from the `telrad-relay-data` volume. It is the
report authorisation record and holds no key material. If it is lost, reports
for orders placed before the loss are refused until the RIS resends those
orders or the clinic opens a [backlog acceptance](#backlog-acceptance) window.

Do not back up or restore `identity.json`. It is the Relay's private key: a
restored copy on another host makes two hosts one Relay, and an old copy may
hold an expired certificate.

To replace a lost host:

1. Copy any ledger backup into the new volume before starting the container.
   Never restore `identity.json`.
2. Pair the new container with a new token.
3. Have a company administrator choose **Replace** on the old Relay in
   Telrad's settings. Telrad revokes the old Relay and moves every outstanding
   and failed report delivery onto the replacement, which it retries.
4. Run `docker compose run --rm relay accept-backlog --hours 72` so those
   reports are accepted and their accessions recorded.

Telrad retries a report Relay answers with `AR` or `AE` on its normal backoff,
about eight attempts over two days.

## Removal

`docker compose down` keeps the named volume. Removing `telrad-relay-data`
destroys the Relay's identity and ledger, so treat it as a separate, approved
destructive step. Never use `docker compose down -v` as an ordinary
troubleshooting step. Ask Telrad to revoke a Relay that is being retired.
