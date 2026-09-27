# Relay testing

The Go suite runs entirely on loopback. It needs no Docker, cloud account or
network access, and uses synthetic identifiers and payloads only.

```bash
go test -race ./...
go vet ./...
govulncheck ./...
scripts/check-licenses.sh
scripts/check-publication.sh
```

Use the Go version pinned by `.github/workflows/ci.yml`. Some managed
development environments restrict loopback sockets; that is an environment
restriction, not a product failure.

## Fake Telrad

Each test creates a throwaway certificate authority standing in for Telrad's.
It issues the fake Telrad listeners' server certificates and signs Relay's
certificate requests the way the enrolment endpoint would. The fake DICOM, HL7
and report listeners require a client certificate from that authority, so every
test that forwards traffic exercises mutual TLS. No private key or certificate
is checked in; all are generated at run time.

## What the tests cover

- **Pass-through.** DICOM bytes out equal DICOM bytes in over mutual TLS,
  including half-close; a clinic connection is closed when Telrad cannot be
  reached; Telrad refuses a client without a Relay certificate; connections
  beyond the limit are refused at accept.
- **HL7 orders and the ledger.** Accessions are recorded only after `AA` to an
  `NW` or `XO` order; `AE`, `AR` and other order controls record nothing, and a
  cancellation does not remove an entry; multi-`OBR` orders record every
  accession; pipelined messages correlate by `MSA-2` rather than arrival
  order; a ledger write failure closes the connection without
  forwarding the `AA`; malformed frames close both sides. MLLP framing, frame
  limits, field parsing with custom separators and multiple `OBR` segments, and
  composed acknowledgements are tested directly.
- **Ledger file.** Appends, duplicate suppression, reload at start and file
  mode `0600`.
- **Report pickup.** An authorised report reaches the receiver unchanged and the
  receiver's `AA` or `AR` is returned byte for byte; unknown, partly known and
  missing accessions are refused with `AR` without contacting the receiver; an
  unreachable receiver or an invalid acknowledgement produces `AE`; pickup
  reconnects after a dropped connection and after certificate renewal; a
  protocol error closes the connection.
- **Pairing and renewal.** Token and link pairing against a loopback enrolment
  server, identity reload and file mode, renewal authenticated by the current
  certificate, renewal inside the 30-day window, rejection of redirects and bad
  responses, and rejection of a corrupt identity file.
- **Configuration and CLI.** Defaults, file and environment precedence,
  environment variable names, validation, unknown-field rejection, `version`,
  `help`, `status` against a stopped service, the status and readiness
  endpoints, container pairing with `enroll`, and a container that is unpaired
  without a token.
- **End to end.** `runRelay` against fake Telrad listeners: it reaches `ready`,
  forwards an order and records its accession, delivers the matching report,
  forwards DICOM bytes and shuts down cleanly.

## CI only

The Windows service, the Linux and Windows installers and the container image
are exercised in CI on disposable runners, not by `go test` on a development
machine. Cross-compiling for Windows does not exercise the Service Control
Manager, and the Linux suite does not exercise systemd. Do not run installer
tests on a clinic host.

Building and testing release artifacts does not authorise publishing, tagging
or promoting them.
