# Release policy and operations

This document prepares the relay for public releases without authorizing a release, deployment, deletion, or visibility change. Complete the pre-public checklist only during an explicitly approved release window.

## Fast public testing

Routine development happens on `dev`, where pushes run CI. Advancing reviewed
work to `main` runs CI and CodeQL but does not publish assets. After those checks
pass, an authorized maintainer explicitly dispatches
`.github/workflows/publish-testing.yml`. That workflow runs the focused
repository checks, builds native binaries, packages local Linux and Windows
installers, and creates a public GitHub prerelease.

No version choice or manual tag is required. The workflow embeds a synthetic
version such as `0.0.0-testing.RUN_ID.ATTEMPT` and automatically creates a
non-SemVer tag such as `testing-RUN_ID-ATTEMPT`, using the unique workflow run
ID because every GitHub Release must refer to a tag. It does not publish a
container, update the container tags `testing` or `latest`, use production
signing material, or make the build eligible for production promotion. Native
artifacts and `testing.json` are signed by a separate testing Ed25519 key. The
signed metadata binds the `testing` channel, embedded version, release tag,
source commit, platform, immutable artifact URL, and digest.

Create a protected GitHub Environment named `testing-release` with:

- the secret `RELAY_TESTING_UPDATE_SIGNING_KEY_BASE64`, containing base64 of the
  testing Ed25519 private PEM; and
- the variable `RELAY_TESTING_UPDATE_PUBLIC_KEY_BASE64`, containing base64 of
  the independently retained matching DER-encoded public key.

Generate the public environment-variable value with:

```bash
openssl pkey -pubin -in update-public.pem -outform DER | base64 | tr -d '\n'
```

The testing key must never be reused for stable releases. Restrict write access
to the environment. A protected environment reviewer can provide a second
approval gate after the maintainer manually dispatches the workflow.
The environment-scoped build job has read-only repository access. A separate
publication job receives the checksummed bundle through a short-lived workflow
artifact and never receives the testing private key.

A simple protected-main branch flow is:

```bash
git switch dev
# Commit and push normal development work to dev.
git push origin dev

# After CI passes and the change is ready for machine testing:
gh pr create --base main --head dev --fill
# Obtain the required review, wait for all required checks, and squash-merge.

# After the merge commit passes CI and CodeQL, explicitly publish testing assets:
gh workflow run publish-testing.yml --ref main
```

Download the standalone bootstrap installer from the newest GitHub prerelease:

```bash
curl -fLO \
  https://github.com/telrad-au/relay/releases/download/testing-RUN_ID-ATTEMPT/install.sh
chmod +x install.sh
./install.sh
# First installation only:
telrad
```

Windows testers run the equivalent commands from an Administrator PowerShell
terminal:

```powershell
irm https://github.com/telrad-au/relay/releases/download/testing-RUN_ID-ATTEMPT/install.ps1 | iex
telrad
```

The Linux bootstrap detects amd64 or arm64 automatically. The bootstrap scripts
download the matching public bundle, then invoke its local installer. Each
archive includes the local installer, development pairing configuration,
installation manifest, administrator-owned testing trust policy, and licence
notices. The release also publishes the three raw updater artifacts,
`testing.json`, detached update signatures, and `SHA256SUMS`. Each bootstrap
pins the exact platform archive digest before extracting it. A Linux bootstrap
upgrade preserves the existing configuration and authentication. It restarts a
service that was running before the upgrade and leaves an already-stopped
service stopped.

After installing one signed testing build, publish a later `testing-*` release
and run:

```bash
telrad update
# Review the exact source commit, release, artifact URL, and SHA-256 printed above.
sudo telrad update 0.0.0-testing.43.1
```

On Windows, run both commands from an Administrator terminal and omit `sudo`.
The first command queries the public GitHub Releases API only to discover the
newest immutable `testing-*` manifest, verifies that manifest with the pinned
testing key, and does not download an executable. The second command succeeds
only when its exact version still matches that signed manifest. Testing trust
rejects stable manifests, and stable trust rejects testing manifests. Moving
between channels requires running a separately reviewed installer.

## Distribution contract

Stable native releases use stable SemVer tags such as `v2.1.0`. The GitHub Release and its assets are immutable after publication. Native binaries are published for Linux amd64, Linux arm64, and Windows amd64. Each release includes:

- SHA-256 metadata and a combined `SHA256SUMS` file;
- detached Ed25519 signatures used by the relay updater;
- a timestamped Azure Artifact Signing Authenticode signature on the Windows
  binary;
- SPDX JSON SBOMs and GitHub SBOM attestations for each binary;
- GitHub build-provenance attestations;
- an in-toto release attestation binding the promoted container digest to the
  stable OCI package version;
- `stable.json` metadata whose per-platform signatures bind the channel,
  version, release tag, source commit, platform, immutable artifact URL, and
  digest, plus trust-pinned installers; and
- project licence, attribution notice, and third-party notices.

GitHub Releases is the only native artifact origin. Prereleases use immutable
exact-tag URLs. Stable installers and `stable.json` are also available through
GitHub's `releases/latest/download/ASSET` redirect, which selects the latest
published non-prerelease release. No GitHub Pages site, application-hosted copy,
or external object store is part of the distribution contract.

The public container package is `ghcr.io/telrad-au/relay`. Its tag contract is deliberately small:

- `MAJOR.MINOR.PATCH-PRERELEASE` is an immutable tested build and records its
  source revision, digest, target stable version, platforms, and publication
  workflow in the `container-promotion.json` GitHub Release asset;
- `MAJOR.MINOR.PATCH` is immutable and must never be replaced;
- `latest` moves only to the newest verified stable release; and
- `testing` moves only to the newest prerelease.

The prerelease workflow builds each multi-architecture image once with its OCI
provenance and SBOM. The stable workflow never rebuilds or changes that image's
OCI contents.
It selects the highest SemVer prerelease for the same stable version and source
commit whose publication workflow and live GHCR manifest still verify, then
copies that exact digest to the immutable stable version tag. After the native
artifacts, stable container tag, and attestations verify, it publishes the
immutable GitHub Release. Only then does it advance the mutable `latest`
container tag. Release notes record the promoted prerelease, source revision,
native verification instructions, container version tag, and immutable
container digest. Container consumers should pin the digest in production.
Because the OCI content is not changed, the relay's embedded version and
`org.opencontainers.image.version` label remain the selected prerelease version;
the stable tag and release attestation record its production promotion.

## Signed development Linux releases

Prerelease tags build a Linux-only signed development bundle in addition to the
container and raw Windows integration artifact. The bundle uses the immutable
GitHub Release URL for its exact prerelease tag and a development Ed25519 trust
root that must never be reused for production. Its `stable.json` channel name
matches the relay update protocol, but its prerelease version, exact-tag
manifest URL, and trust root keep it separate from the production channel.

Create a GitHub Environment named `development-release`. Configure these
environment secrets:

- `RELAY_DEV_UPDATE_SIGNING_KEY_BASE64`: base64 of the development Ed25519
  private PEM; and
- `RELAY_DEV_UPDATE_PUBLIC_KEY_BASE64`: base64 of the independently retained
  matching public PEM.

Configure `RELAY_DEV_ENROLLMENT_URL` as an environment variable containing the
complete HTTPS development Relay pairing endpoint. The protected variable name
is retained for release-workflow compatibility; generated schema-v3
configuration writes it as `pairingUrl`.

The official container's production pairing endpoint is source-controlled in
`packaging/docker-relay.json`, included in the prerelease evidence, and retained
when that exact digest is promoted without rebuilding. Container deployments
may override it at runtime with `TELRAD_RELAY_PAIRING_URL` for development or a
self-hosted control plane. The stable workflow refuses publication unless the
source-controlled container endpoint exactly matches the
`production-release` environment's `RELAY_ENROLLMENT_URL`.

The environment-scoped build job has read-only repository access. It refuses
missing values and non-HTTPS URLs, validates that the key pair matches, signs
the canonical Linux digest files and per-platform update statements, renders a
trust-pinned `install.sh`, and verifies the generated bundle. A separate job
receives the checksummed bundle through a short-lived workflow artifact, builds
and publishes the container, and creates the GitHub prerelease without access
to the development private key. The Windows prerelease binary remains
checksum-only and must not be presented as a signed development installer.

Install by selecting the exact prerelease tag:

```bash
curl -fsSL https://github.com/telrad-au/relay/releases/download/v2.1.0-rc.1/install.sh | sudo sh
telrad
```

The development application may link to that exact URL through its own
environment configuration. `telrad update` checks the manifest at the same
exact tag, so this release-qualification path does not advance when another
prerelease is published. Creating a tag does not deploy or modify the
development application or alter production. If the development private key is ever placed in a plain
GitHub variable, log, artifact, or repository file, delete it and rotate the
development key pair before publishing another bundle.

## Protected GitHub configuration

Before creating the first stable tag:

1. Enable only squash merging for pull requests. Disable merge commits and
   rebase merging so protected `main` remains linear.
2. Enable a `main` ruleset that requires a pull request, one approval,
   code-owner review when applicable, resolved conversations, linear history,
   and the Linux, Windows, and primary CI checks. Block force pushes and branch
   deletion, and do not allow administrator bypass.
3. Enable a release-tag ruleset for `refs/tags/v*.*.*` that restricts tag
   creation to release maintainers and blocks tag updates, deletion, and
   non-fast-forward changes. Stable and prerelease tags are immutable release
   identities.
4. Create a GitHub Environment named `production-release`.
5. Require an independent reviewer and prevent self-review. Restrict deployment tags to stable release tags. Do not allow administrator bypass where the account plan supports those controls.
6. Provision Azure Artifact Signing before adding the environment values:
   - create an Artifact Signing account, complete the `Telrad Pty Ltd` public
     identity validation, and create a `PublicTrust` certificate profile;
   - create a Microsoft Entra application with a GitHub federated credential
     whose subject is
     `repo:telrad-au/relay:environment:production-release` and whose audience is
     `api://AzureADTokenExchange`; and
   - grant that identity the `Artifact Signing Certificate Profile Signer` role
     at the narrowest available scope. Do not create an Azure client secret.
   Follow Microsoft's [Artifact Signing
   quickstart](https://learn.microsoft.com/en-us/azure/artifact-signing/quickstart)
   and [GitHub OIDC
   guidance](https://learn.microsoft.com/en-us/azure/developer/github/connect-from-azure-openid-connect)
   for the account and federated-identity setup.
7. Configure these environment secrets:
   - `RELAY_UPDATE_SIGNING_KEY_BASE64`: base64 of the Ed25519 private PEM;
   - `RELAY_UPDATE_TRUSTED_PUBLIC_KEY_BASE64`: base64 of the independently retained Ed25519 public PEM;
   - `AZURE_ARTIFACT_SIGNING_CLIENT_ID`: the Entra application's application
     (client) ID;
   - `AZURE_ARTIFACT_SIGNING_TENANT_ID`: the Microsoft Entra tenant ID;
   - `AZURE_ARTIFACT_SIGNING_SUBSCRIPTION_ID`: the Azure subscription ID;
   - `AZURE_ARTIFACT_SIGNING_ENDPOINT`: the regional Artifact Signing account
     endpoint, such as `https://eus.codesigning.azure.net/`;
   - `AZURE_ARTIFACT_SIGNING_ACCOUNT_NAME`: the Artifact Signing account name;
     and
   - `AZURE_ARTIFACT_SIGNING_CERTIFICATE_PROFILE_NAME`: the `PublicTrust`
     certificate profile name.
   The Azure identifiers are not private keys, but they remain environment
   secrets to match the isolated signing pattern used by the upstream Codex
   workflow.
8. Configure this environment variable:
   - `RELAY_ENROLLMENT_URL`: the production Relay pairing endpoint (the
     protected variable name is retained for release-workflow compatibility).
9. Enable GitHub private vulnerability reporting.
10. Enable GitHub immutable releases. This must happen before the first stable release because it applies prospectively.

The read-only `plan` job runs before the environment gate and records the stable
tag, source revision, selected prerelease, digest, and platforms in the workflow
summary. Review that summary before approval. An unprotected job builds and
tests the unsigned native bundle first. The protected Windows job then uses
GitHub OIDC to obtain a short-lived Azure identity, applies the Artifact Signing
signature and Microsoft RFC 3161 timestamp, verifies Authenticode, derives the
signer certificate pin, and applies Relay's Ed25519 signatures. It has read-only
repository access and cannot publish a release or package. A later unprivileged
job creates SBOMs and attestations from that signed bundle. The publishing job
receives no production signing secret, reselects the candidate after approval,
and fails if any approved identity changed. Approving the protected signing job
authorizes production signing and its dependent automatic publication if every
verification succeeds. The workflow also checks repository visibility, GHCR
visibility, annotated and monotonically increasing stable tags, membership of
the release commit on `main`, release immutability, and existing version
collisions before publication.

## Release identity

Every published tag and version identifies exactly one source revision and set
of artifacts. Create `testing` only through the reviewed testing workflow, and
never reuse an issued version number for different artifacts.

## Preparation while private

These steps can be completed before the public-release window:

1. Keep `main` CI green and review the intended release commit.
2. Verify that `LICENSE` remains the unmodified Apache License 2.0 text, the
   complete distributed dependency graph is represented in
   `THIRD_PARTY_NOTICES.md`, and public documentation does not add restrictions
   to the Apache-2.0 grant.
3. Provision the Ed25519 keypair outside the repository, confirm the retained
   public key matches it, and complete the Azure Artifact Signing identity,
   profile, OIDC, and role setup described above. Production never exports an
   Authenticode private key or stores a PFX in GitHub.
4. Create the `production-release` Environment and configure its secrets and variables. If the account plan does not offer required reviewers for private repositories, add that protection immediately after the visibility change and before creating a stable tag.
5. Audit Git history, Actions logs, issues, and package metadata for credentials, patient data, internal hostnames, and other material unsuitable for disclosure.
6. Confirm the `ghcr.io/telrad-au/relay` package is absent or private and that
   no test installation relies on a legacy repository or package namespace.
7. Choose the reviewed stable version and commit, but do not create or push the stable tag yet.

## Public cutover checklist

Complete these in order during the approved public-release window:

1. Reconfirm the reviewed commit, green CI, prerelease and stable versions,
   environment secret names, signing-key fingerprints, and that the
   source-controlled container pairing endpoint exactly matches the production
   `RELAY_ENROLLMENT_URL`.
2. Confirm the intended release tags and package versions do not already exist.
3. Change the repository to public and confirm its history and workflow logs render as expected.
4. Enable immutable releases and private vulnerability reporting. Add the environment reviewer and deployment-tag restriction now if the account plan did not permit them while private.
5. Change the GHCR package to public and verify an unauthenticated manifest pull.
6. Create and push an annotated prerelease SemVer tag from the reviewed commit.
   Wait for the prerelease workflow to succeed, test the immutable versioned
   image by digest, and retain the exact commit SHA. Do not use `testing` as
   promotion evidence.
7. Create and push the annotated stable SemVer tag at that exact same commit.
   If the source changed, publish and test another prerelease first. Do not
   reuse or move a release tag.
8. Open the completed `Plan stable release` summary. Approve the
   `production-release` job only after reviewing its stable tag, source commit,
   selected prerelease and digest, platforms, environment, and signing inputs.
   That approval authorizes both production signing and the dependent automatic
   publication job.
9. Verify the completed GitHub Release, attestations, native signatures, and
   public unauthenticated downloads. Confirm the prerelease version tag, stable
   version tag, and `latest` container tag all resolve to the recorded digest.
   Confirm `/releases/latest/download/install.sh`, `install.ps1`, and
   `stable.json` resolve to the new stable release assets.
10. Attach both workflow URLs, commit, tags, checksums, image digest, and
    verification output to the release record.

The workflow does not change repository or package visibility. Publishing the
stable GitHub Release is the native update-channel promotion.

## Independent verification

Download a release into an empty directory, then verify its checksums:

```bash
sha256sum --check SHA256SUMS
```

Verify GitHub provenance and SBOM attestations for each native binary:

```bash
gh attestation verify telrad-relay-linux-amd64 \
  --repo telrad-au/relay
gh attestation verify telrad-relay-linux-amd64 \
  --repo telrad-au/relay \
  --predicate-type https://spdx.dev/Document/v2.3
```

Verify the container using the digest recorded in the release notes:

```bash
docker buildx imagetools inspect \
  ghcr.io/telrad-au/relay@sha256:DIGEST
gh attestation verify \
  oci://ghcr.io/telrad-au/relay@sha256:DIGEST \
  --repo telrad-au/relay
```

On Windows, confirm that `Get-AuthenticodeSignature` reports `Valid`, the signature chains to the expected publisher, and the RFC 3161 timestamp is present. The release workflow additionally runs the relay's own generated-bundle test, which verifies the Ed25519 signatures, manifest digests, pinned update public key, pinned Windows signer certificate, installers, and required notices before publication.

After publication, install through GitHub's latest-stable URLs on clean Linux
amd64, Linux arm64, and Windows amd64 test hosts. Enroll each relay, run
`telrad doctor`, and confirm that the latest manifest references the immutable
tagged GitHub Release artifacts.

## Tagging a prerelease and stable release

Record the intended source commit once and use it for both annotated tags. For
example, to build `v2.1.0-rc.1` and later promote it to `v2.1.0`:

```bash
git switch main
git pull --ff-only origin main
release_commit="$(git rev-parse HEAD)"
git tag --annotate v2.1.0-rc.1 "$release_commit" --message "Telrad Relay 2.1.0 release candidate 1"
git push origin refs/tags/v2.1.0-rc.1
```

After the prerelease workflow succeeds and that digest passes integration
testing, resolve the prerelease tag again and create the stable tag from that
exact commit:

```bash
git fetch origin refs/tags/v2.1.0-rc.1:refs/tags/v2.1.0-rc.1
release_commit="$(git rev-parse 'v2.1.0-rc.1^{commit}')"
git tag --annotate v2.1.0 "$release_commit" --message "Telrad Relay 2.1.0"
git push origin refs/tags/v2.1.0
```

Confirm that `release_commit` matches the SHA recorded in
`container-promotion.json` and the prerelease workflow summary. The stable
workflow fails closed if the commits differ or the prerelease publication and
digest cannot still be verified. Both release workflows require annotated tags
whose commits are on `main`; lightweight or off-main release tags are rejected.

The prerelease is the safe end-to-end qualification run. Use a new release
candidate such as `v2.1.0-rc.2`, exercise its native bundle and container by
digest, and fix any issue with a higher prerelease tag. Do not create the stable
tag until that exact commit and digest are accepted. Creating the stable tag
starts the production workflow, but all mutation and access to production
signing material remain blocked behind the independently reviewed environment
job.

## Failure before publication

The workflow uploads a short-lived recovery copy of the verified native bundle
before it creates the immutable stable container tag. It publishes the GitHub
Release only after the immutable outputs verify and advances the mutable
container `latest` tag as its final mutation. If a later step fails:

- do not replace an existing version tag or release asset;
- leave the GitHub Release unpublished if it is still a draft;
- inspect the workflow recovery artifact and logs;
- if the immutable stable container tag was already created, do not move it;
  either finish the same release from the preserved bundle after review or
  abandon that version and release a new patch version; and
- do not publish the draft GitHub Release or move the container `latest` tag
  unless the entire release verifies.

If the immutable GitHub Release was published successfully and only the final
mutable-tag step failed, verify the recorded stable tag and digest again, then
an authorized release maintainer can finish that exact channel update with:

```bash
scripts/promote-container.sh promote-latest \
  ghcr.io/telrad-au/relay vMAJOR.MINOR.PATCH sha256:DIGEST
```

## Rollback and release revocation

Released native binaries and immutable container version tags are never
overwritten. Recovery is a forward release:

1. Stop recommending or installing the affected release and preserve its
   manifest and release evidence.
2. Mark the GitHub Release as affected in its notes or security advisory. If
   continued public download is unsafe, delete the release under the incident
   decision process; never reuse its tag or version.
3. If the container `latest` tag points to the affected image, move it to the
   last known-good digest or remove it. Keep the affected immutable version tag
   for investigation unless active distribution risk requires package-version
   removal.
4. Build, sign, verify, and publish a higher patch version as soon as possible.
   GitHub's latest-release URLs then advance to that fixed release.
5. Notify affected customers with the revoked version/digest, impact,
   mitigation, fixed version, and required reinstall or upgrade action.

For an Ed25519 update-key compromise, delete the affected release if continued
download is unsafe, revoke access to the signing environment, generate a new
offline keypair, and publish new trust-bootstrap installers in a higher release.
Existing installations pin the old key and require an explicitly managed trust
transition or reinstall.

For an Authenticode identity or certificate compromise, disable the Entra
federated credential and its Artifact Signing role, revoke or replace the
affected certificate profile through Azure, rotate the public Azure identifiers
stored in GitHub where applicable, update the pinned certificate digest in newly
generated installers, and require reinstall where the old pin cannot be safely
rotated. There is no production PFX or Azure client secret to rotate.

For a GitHub Actions or GHCR compromise, disable the release workflow, revoke relevant tokens and sessions, compare published digests with retained release evidence, remove mutable tags, and publish a security advisory before resuming releases.

Every rollback or revocation records who authorized it, timestamps, affected versions and digests, actions taken, customer communications, and the replacement release.
