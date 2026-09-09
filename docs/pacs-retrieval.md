# Accession-authorized PACS retrieval (v2)

Retrieval is opt-in. Existing installations continue raw HL7, DICOM push and
report return after the schema-v4 to v5 upgrade. Company Push remains the default;
Retrieve also accepts ordinary pushed DICOM. Do not publish a release or enable
`RELAY_PACS_RETRIEVAL_ENABLED` as part of installing this change. Activation
requires the actual Relay build, clinic source feed, PACS profile and cloud
receipt pipeline to pass qualification together.

## Authority and storage

A locally approved ORM O01 referral grants permanent access to every study under
its exact accession and accession issuer on one local PACS. Patient identity is
not required and does not narrow newly signed permits. Each OBR gets one permit with its ordinal and source-set ID. ZDS does not
narrow that grant. An old permit may discover later studies and images, including
after successful retrieval and a Relay restart. `issuedAt` is metadata, not a
freshness check. Retiring its locally trusted key stops further use.

The cloud retains referrals, original signatures, jobs, leases, selections,
receipts and outcomes. Relay keeps active work in memory. It adds only the permit
signing key to its existing protected configuration/credential storage. There is
no authorization ledger, queue, SOP deduplication, clinical spool, recovery file,
or background PACS scheduler. Runtime diagnostics contain only operational state.

Company selection, registration of a public key and unsigned claim metadata do
not establish local authority. The cloud cannot choose a PACS URL, provision a
company identity, change namespaces or install a trusted verification key.

The unreleased v2 contract is accession-only. Permits containing a `patient`
field are rejected; there is no patient-bound compatibility mode.

The configured RIS/PACS accession namespace must uniquely identify orders over
the grant lifetime. If accessions are reused, rotate the approved namespace/PACS
identity and retire affected trust before retrieval; patient metadata is not a
collision check for new permits.

## Provisioning and migration

Schema v4 upgrades automatically to v5 with retrieval absent and push unchanged.
Read-only commands report that migration is needed and do not write the file.
Schema v3 retains its existing polling migration and advances to v5. Installers
preserve existing configuration, credentials and permit keys. Older binaries
reject v5; rollback requires the previous configuration as well as the previous
binary. Disable retrieval and drain work first. Never restore a local job ledger.

Pairing supplies the Relay ID. Separately obtain and approve the company's opaque
ID through the clinic's platform administrator; set `companyId` and `connectorId`
locally. `connectorId` must equal the paired `relayId`. Discovery compares these
values; it never copies them into configuration. Re-pairing to another identity
requires explicit reprovisioning of the clinic authorization configuration.

With the service stopped, run under the identity that owns protected Relay state:

```sh
telrad --config /protected/relay/relay.json retrieval-keygen
```

This creates `permit-signing-key.json` beside the configuration using exclusive
creation. It refuses to overwrite an existing file and prints no key material.
Unix requires a `0700` state directory and `0600` key file. Windows uses the same
private file-creation and service ACL mechanisms as credentials. Native installer
permission repair includes the named key file. In a container, generate it as
UID 10001 in the persistent state volume. Do not put it in an image, environment
variable, installer bundle, ticket or log.

Read only the `publicKey` and `keyId` fields through a protected administrator
workflow. Approve the public key in `trustedPublicKeys` and its ID in
`signingKeyId`. The ID is `ed25519-` followed by the lowercase SHA-256 of the raw
public key, making replacement under the same ID impossible. Relay registers
only that public key at `/v1/relay/signing-keys` before submitting a referral.
Registration does not modify local trust. The signing key is independent of
bearer credentials and release-verification keys.

Add this object to a paired schema-v5 configuration after replacing placeholders:

```json
{
  "retrieval": {
    "enabled": true,
    "companyId": "APPROVED_COMPANY_ID",
    "connectorId": "PAIRED_RELAY_ID",
    "signingKeyId": "LOCAL_KEY_ID",
    "trustedPublicKeys": ["LOCAL_PUBLIC_KEY_BASE64URL"],
    "pacs": [{
      "id": "clinic-pacs-1",
      "dicomwebUrl": "https://pacs.example.invalid/dicom-web",
      "accessionIssuer": "CLINIC",
      "adapter": "dicomweb-qido-wado-v1",
      "maxStudyBytes": 1073741824,
      "maxInstanceBytes": 268435456,
      "maxInstances": 10000,
      "requestTimeoutSeconds": 300
    }],
    "policies": [{
      "id": "clinic-orm-v1",
      "pacsId": "clinic-pacs-1",
      "sourceAddresses": ["192.0.2.20"],
      "sendingApplication": "RIS",
      "sendingFacility": "CLINIC",
      "orderControls": ["NW", "XO", "CA", "DC"],
      "accessionSource": "OBR-18"
    }]
  }
}
```

Set `disableDicomListener: true` at the configuration root when inbound DICOM is
unneeded. Retrieval uses no DICOM listener or PACS-initiated connection. Leave
this setting absent to allow push and retrieval together. Listener edits require
restart. Policy/trust edits are rechecked before work, every second during an
attempt and during renewal; control refreshes capability advertisement after a
valid local configuration edit. Drain/restart for coordinated provisioning changes.
Never reuse a PACS or source-policy ID for another endpoint, namespace or mapping.

## Clinic feed and routing

This profile supports `ORM^O01` in HL7 2.3.1 and 2.5 with standard `|^~\&`
delimiters and explicit processing ID P or T. Configure exactly one matching
source policy per feed. The actual TCP peer must match an approved source IP;
MSH application/facility values are additional restrictions, not authentication.
Enforce host-firewall source restrictions and prevent address spoofing on the
clinic network. Do not authorize NAT/proxy addresses shared with unapproved feeds.
Cloud report delivery never invokes the signer. For opted-in clinics, report
return accepts only ORU R01 and rejects embedded messages/framing, preventing a
cloud-origin ORM from being returned through the RIS referral feed.

PID-3 may be blank, absent, repeated or use another patient namespace. It remains
in the original HL7 for clinical processing, but is omitted from new permits.
OBR-18 is the default accession mapping. OBR-3, ORC-3, OBR-2 and ORC-2
require explicit matching clinic/cloud mappings; this profile accepts their local
EI identifier/namespace components, rejecting unsupported compound issuers.
Mixed ORC controls and multiple PID segments are rejected. CA/DC sign zero
new permits. A whole-message error must not silently drop one OBR.

The coordinated cloud addition is:

`GET /v1/relay/control/retrieval-settings`

It uses the existing bearer and exactly one protocol-version `1` header, and
returns uncached `{version:2,companyId,connectorId,ingestMode,mode,available}`.
`mode` is PUSH or RETRIEVE; `available` is the independent platform switch. Each
new ORM exchange discovers its route explicitly. Push uses raw HL7; Retrieve uses
the signed envelope. Missing/malformed discovery, identity mismatch, unavailable
retrieval or a failed signed request closes the exchange without unsigned
fallback. TEST remains in the cloud test lane and never starts PACS retrieval.
The companion cloud change must be deployed before opting a clinic in. An idle
poll or a `retrieval_not_enabled` response is not mode discovery.

The chosen route, body and idempotency key remain fixed across transport retries.
The exact correlated AA/AE/AR is returned after cloud processing, without waiting
for images. AE/AR remain negative application acknowledgements. A mode change
can reject in-flight signed work; sender retry/reconciliation is required.

## Qualified adapter profile

The initial adapter uses study-level QIDO-RS and complete-study WADO-RS over a
locally configured HTTPS base URL, with system certificate verification and no
redirects. PACS requests use a separate direct transport with no cloud bearer or
environment proxy. This profile expects a clinic endpoint that requires no
additional HTTP authentication. Other PACS authentication/profile requirements
need separate qualification; never put credentials in the URL.

QIDO must support exact Accession Number and
Issuer of Accession Number Sequence (`00080051.00400031`), and return all those
identity values plus Study Instance UID. The accession issuer uses exactly one
Local Namespace Entity ID item. Missing/conflicting identity is rejected. The
server must honor `limit=65` and signal truncation with Warning; Relay rejects
warnings, pagination links, duplicate study rows, and more than 64 studies. It
never silently truncates. All current selected UIDs must be confirmed by the
cloud before WADO starts; later attempts query again.

WADO must return a complete `multipart/related` response containing Part 10
objects. The parser supports Explicit VR Little Endian and the existing listed
encapsulated transfer syntaxes encoded with explicit little-endian elements.
Implicit VR, Big Endian and deflated datasets require a separately qualified
parser and are rejected by this adapter. Dataset elements must be ordered;
identity metadata is bounded to 1 MiB. Namespaces use ASCII or UTF-8 and must be
present in the object itself. No byte conversion or transfer-syntax negotiation
that requests transcoding is performed.

The adapter has no standardized acquisition-completion query: it reports
`not_provided` and uses the signed order under the fixed PACS-or-order rule.
This is an adapter limitation, not a configurable readiness policy. It does not
interpret Orthanc stability timers, retired status tags or instance counts as
completion. A PACS with a vendor completion API or expected SOP inventory needs
an adapter that queries and preserves that evidence before qualification. Failed
QIDO/WADO responses never become successful order fallback.

Every object gets a separate HTTPS upload and a fresh valid HTTP 201 receipt,
including retransmissions. Only retrieval uploads carry the active attempt ID.
Successful results cover all selected studies with positive distinct SOP counts,
sorted-unique LF-joined SHA-256 inventories and zero outstanding uploads. A bad
object, truncated multipart/HTTP response, unconfirmed receipt or failed study
prevents whole-attempt success. One serial worker, explicit size/count/time limits,
cloud backpressure, renewable leases and cancellation bound resource use.

## Recovery and qualification

`status` separates retrieval availability from ingest/report readiness; `doctor`
checks local signing-key/configuration blockers. Neither queries PACS. Investigate
configuration, trust, authentication and PACS profile errors locally. Do not attach
permits, HL7, patient identifiers, DICOM UIDs, keys or tokens to operational output.
The cloud owns retry scheduling, manual retry and durable outcomes. After crash,
let its lease expire and accept a fresh claim with the original signed permit.

For key rotation, stop/drain the service, protect/archive the existing key file,
explicitly generate a new one, and approve its public key and ID in configuration.
Keep old public keys only while the clinic authorizes their existing grants.
Removing an old public key retires those grants locally. Credential rotation never
rotates signing authority. Lost-key recovery either restores an approved protected
backup with the identical public key or creates a new authority and obtains fresh
clinic referrals. Never reconstruct approval from unsigned jobs.

See [testing](testing.md#retrieval-v2) for executable evidence and the remaining
joint platform qualification gate. Keep the platform switch disabled until that
gate and the clinic's exact version/PACS approval are complete.
