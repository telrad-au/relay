# Order-authorized report return

Relay requires a clinic-signed report permit before sending a report to the RIS.
This applies in both Push and Retrieve modes. The permit authorizes an accession
within an approved RIS namespace and binds the company's ID, Relay ID, source
policy and local report host/port. Patient ID is not an authorization requirement.

The clinic's approved HL7 feed is the authority. Relay signs each OBR in an
approved ORM O01 order using its protected Ed25519 key. Telrad retains the
original permit with the order, attaches it when queuing a report, and returns
it with the report claim. Relay verifies the signature using locally approved
keys and checks the report's exact OBR-18 accession before opening an MLLP
connection. Neither cloud credentials, public-key registration nor a cloud
capability response can turn this check off.

## Local setup

Existing enabled `retrieval` configuration supplies the company, source policies
and trusted key when `reportAuthorization` is absent. This works while Telrad's
image transfer mode is Push too. A separate report configuration is useful for
Push-only clinics or a separately approved order feed.

With the service stopped, provision the protected key using the existing command:

```sh
telrad --config /protected/relay/relay.json retrieval-keygen
```

Reuse an existing `permit-signing-key.json`; the command refuses to overwrite it.
The command name is retained for compatibility and the same key signs two
cryptographically distinct purposes. Follow the [key provisioning instructions](pacs-retrieval.md#provisioning-and-migration)
for Unix modes, Windows ACLs and container UID 10001. Never copy the private
`seed` into configuration, logs or the cloud. Approve only its `publicKey` and
`keyId` fields locally, together with the company ID and paired Relay ID.

Add the following to a paired schema-v5 configuration, replacing placeholders:

```json
{
  "reportHost": "192.0.2.20",
  "reportPort": 2576,
  "reportAuthorization": {
    "companyId": "APPROVED_COMPANY_ID",
    "connectorId": "PAIRED_RELAY_ID",
    "signingKeyId": "LOCAL_KEY_ID",
    "trustedPublicKeys": ["LOCAL_PUBLIC_KEY_BASE64URL"],
    "policies": [{
      "id": "clinic-orders-v1",
      "sourceAddresses": ["192.0.2.20"],
      "sendingApplication": "RIS",
      "sendingFacility": "CLINIC",
      "accessionSource": "OBR-18",
      "accessionIssuer": "CLINIC"
    }]
  }
}
```

If both configurations are present, their company and signing-key IDs must agree.
`connectorId` must equal `relayId`. Source policy IDs must remain associated with
the same feed and namespace. Exactly one policy must match the TCP peer address
and MSH sending application/facility. Restrict the listener with clinic firewall
rules; MSH fields alone do not authenticate a sender. Do not approve a shared
proxy/NAT address carrying untrusted orders.

The order profile accepts HL7 2.3.1 and 2.5 ORM O01 with standard delimiters and
processing ID P or T. NW and XO authorize each accession; CA and DC mint no new
permits. TEST-lane orders never authorize live report delivery. Supported source
mappings are OBR-18, OBR-3, ORC-3, OBR-2 and ORC-2. Namespace components, when
present in an EI field, must match `accessionIssuer`.

Restart Relay after provisioning, submit an approved order and confirm its
correlated AA. Finalize a synthetic report and verify that Telrad records the
RIS's correlated application AA. Configure the RIS to resolve returned reports
by OBR-18 within this namespace: Relay accepts exactly one OBR and rejects
conflicting OBR/ORC identifiers, embedded messages and MLLP framing. A missing or
invalid permit produces `invalid_report` without contacting the RIS.

## Deployment and recovery

Deploy cloud support first, then install the reviewed Relay build. Cloud support
has no report-authorization feature flag and works independently of the PACS
retrieval feature switch. New Relay sessions advertise `reportAuthorizationV1`;
Telrad leaves reports without a permit queued. Older Relay versions retain their
previous delivery behavior during cloud rollout and do not provide this guarantee.
Merging a PR does not publish a release or upgrade an installed Relay.

This is an additive schema-v5 setting. Existing configuration and credentials
remain readable, but the new binary will not deliver unsigned reports. Provision
local approval before upgrading report-return installations. Existing orders
need a clinic resend through the approved feed to obtain a permit; the cloud
cannot manufacture one from an older order record. Use a fresh HL7 message control
ID for reauthorization after a key/destination change or an older signed retrieval
receipt: retries of an already accepted signed envelope retain its original scope. When retrying a previously
failed delivery, obtain the permit before using Telrad's report retry action.

Local trust and policy are reloaded before every delivery. Retiring a key or
removing its policy blocks its permits. Changing the report destination requires
new permits and a service restart; it cannot redirect an old grant. Retain old
public keys only while their permits remain approved. Signing timestamps are
metadata, not expiration dates. Order cancellation does not revoke previously
issued permits. Accessions must remain unique within the namespace for the
permit lifetime; do not reuse them under still-trusted permits.

Relay stores no clinical authorization ledger or report spool. Cloud claims and
RIS acknowledgement rules continue to govern retries and amendments. A lost
result can cause the same report to be sent again, so the RIS must handle
retransmissions idempotently.

## Security boundary

A compromised Telrad cannot use this Relay to deliver a report for an accession
outside a valid clinic permit, assuming the clinic host, key, approved order feed
and RIS accession mapping remain trusted. It can still fabricate report content,
alter patient metadata or replay reports for an authorized accession. The order
permit does not prove radiologist authorship, clinical correctness or approval
of a particular report version. Independent report-content signing would be a
separate control.
