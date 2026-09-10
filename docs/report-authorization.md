# Order-authorized report return

Report authorization is built into Relay and always enabled in both Push and
Retrieve modes. Pair Relay and configure the usual RIS report address and port;
there is no authorization setting, strategy selection or key-provisioning step.

Before opening its clinic listeners, Relay automatically creates a protected
Ed25519 key in `report-signing-key.json` beside its credential file. It keeps that
key across restarts and upgrades and registers only the public key with Telrad.
The private key never leaves the clinic. PACS retrieval configuration and its key
are independent of report authorization.

For every ORM O01 order received on the HL7 listener, Relay signs each OBR-18
accession. Telrad stores the original authorization with the order and returns
it with the report. Relay verifies its local signature, paired Relay ID, exact
accession, production processing ID and configured report destination before
opening an MLLP connection. Missing or invalid authorization produces
`invalid_report` without contacting the RIS. Cloud capabilities cannot disable
this check, and Telrad never claims unsigned report deliveries.

The signed envelope also binds the original order's SHA-256, procedure sequence,
source set ID and signing time. The accession namespace is the paired Relay;
patient ID is not an authorization requirement. Orders use HL7 2.3.1 or 2.5 ORM
O01 with standard delimiters and processing ID P or T. NW and XO authorize each
OBR-18 accession; CA and DC issue no new grants. TEST orders cannot authorize
production reports. Reports contain exactly one OBR with the authorized accession
in OBR-18; alternate OBR/ORC identifiers and embedded messages are rejected.

## Operation and recovery

Keep the existing protected Relay state directory or container volume across
restarts and upgrades. Native installers protect the report key alongside the
credential file. Corrupt, linked or insecure key files cause startup to fail;
Relay never overwrites them automatically. Read-only verification never creates
keys. No clinical authorization ledger or report spool is stored locally.

Submit an order, confirm its correlated AA, then finalize its report in Telrad.
Telrad records delivery only after the RIS returns its correlated application AA.
A lost result may cause a retransmission, so the RIS must handle duplicates.

Orders without a signed grant require another order submission from the clinic.
Changing the report destination requires fresh grants and a service restart.
Loss or replacement of the key invalidates earlier grants; re-pairing does not
allow the new Relay identity to use the old identity's grants. Use a fresh HL7
message control ID for reauthorization: retries of an accepted signed envelope
retain its original scope. Grants do not expire automatically, and cancellation
does not revoke an already issued grant. Keep accessions unique within the RIS
for their lifetime.

Cloud support must be deployed before installing this Relay build. There is no
unsigned compatibility mode. Merging a PR does not publish a release or upgrade
an installed Relay.

## Security boundary

The clinic-facing HL7 listener is the order authority. Keep it reachable only by
trusted clinic systems using the installation's network and firewall boundaries.
MSH sender names are not authentication. Anyone who can inject orders into that
listener can authorize their accessions; a cloud report response cannot do so.

A compromised Telrad cannot deliver a report through Relay for an accession
without a valid locally signed order, assuming the clinic host, key, order feed
and RIS accession interpretation remain trusted. It can still fabricate report
text, alter patient metadata or replay reports for an authorized accession. The
grant does not establish radiologist authorship or approval of a report version.
