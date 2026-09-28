# Release policy and operations

This document describes how Relay is released. It does not authorise a release,
deployment, deletion or visibility change. Pushing, merging, tagging,
publishing, promoting a container and changing visibility are separate
authorisation boundaries.

## Release policy

Routine development happens on `dev`, where pushes run CI. Reviewed work
reaches `main` through a squash-merged pull request that passes CI and CodeQL.
Merging to `main` publishes only an unsigned development [main
build](#main-builds).

Releases are cut from `main` with annotated SemVer tags:

- a prerelease tag such as `v2.1.0-rc.1` publishes a GitHub prerelease and an
  immutable prerelease container image;
- a stable tag such as `v2.1.0` at the same commit promotes that tested
  prerelease container without rebuilding it and publishes the stable GitHub
  Release.

Every tag and version identifies exactly one source commit and one set of
artifacts. Never reuse or move a release tag or version.

Relay does not update itself. Operators update by rerunning the installer for
the wanted version or by pulling a new image, so publishing a release changes
no installed Relay.

## Distribution contract

GitHub Releases is the only native artifact origin. Stable releases are
immutable after publication and include:

- `telrad-relay` binaries for Linux amd64, Linux arm64 and Windows amd64;
- `install.sh` for Linux and `install.ps1` for Windows;
- a combined `SHA256SUMS` file;
- a timestamped Azure Artifact Signing Authenticode signature on the Windows
  binary;
- SPDX JSON SBOMs, GitHub SBOM attestations and GitHub build-provenance
  attestations for each binary;
- an in-toto release attestation binding the promoted container digest to the
  stable version; and
- the project licence, attribution notice and third-party notices.

Prereleases use their exact tag URLs, for example
`https://github.com/telrad-au/relay/releases/download/v2.1.0-rc.1/install.sh`.
Stable installers are also available through GitHub's
`releases/latest/download/ASSET` redirect, which selects the latest published
non-prerelease release. No other host is part of the distribution contract.

Unsigned `main-*` development prereleases also exist, one per `main` commit;
see [Main builds](#main-builds). They are not releases in the sense above.

The production enrolment endpoint is source-controlled in
`cmd/telrad-relay/config.go` and embedded in official binaries and images.

The public container package is `ghcr.io/telrad-au/relay`, for Linux amd64 and
arm64:

- `MAJOR.MINOR.PATCH-PRERELEASE` is an immutable tested build. The
  `container-promotion.json` GitHub Release asset records its source revision,
  digest, target stable version, platforms and publication workflow;
- `MAJOR.MINOR.PATCH` is immutable and must never be replaced;
- `latest` moves only to the newest verified stable release.

The prerelease workflow builds each multi-architecture image once with its OCI
provenance and SBOM. The stable workflow selects the highest verified
prerelease for the same stable version and source commit and copies that exact
digest to the stable version tag. It does not rebuild or change the image, so
the embedded version and `org.opencontainers.image.version` label remain the
prerelease version; the stable tag and release attestation record the
promotion. After the native artifacts, stable container tag and attestations
verify, it publishes the GitHub Release, and only then advances `latest`.
Release notes record the promoted prerelease, source revision, container
version tag and digest. Container consumers should pin the digest in
production.

## Main builds

After CI succeeds for a push to `main`, `publish-main.yml` publishes that
commit as a GitHub prerelease, for installing the current `main` on
a test host. No tag or approval is needed.

- The tag is `main-<build>-g<sha7>` and the version `0.0.0-main.<build>.g<sha7>`,
  where `<build>` is `git rev-list --count` of the commit on linear `main` and
  `<sha7>` its short SHA, for example `main-842-gb39dfa0`.
- It holds the native binaries, installers, licences and `SHA256SUMS` only: no
  container image, Authenticode signature, SBOM or attestation. It is unsigned
  and not for clinical use.
- The binaries enrol with the development Telrad,
  `https://ingest.dev.app.telrad.com.au/v1/relay/enrolments`.
- It is never marked latest and is not pruned. It can never be promoted,
  because the prerelease, stable and promotion workflows read only `v*` tags.
- Rerunning the workflow for a published build changes nothing.

The regular installers install main builds. Pass `main` for the newest main
build or `main-<build>` for a given one (`-Version` on Windows, or
`TELRAD_RELAY_VERSION` on either):

```bash
curl -fsSL https://raw.githubusercontent.com/telrad-au/relay/main/packaging/install.sh | sudo sh -s -- main
curl -fsSL https://raw.githubusercontent.com/telrad-au/relay/main/packaging/install.sh | sudo sh -s -- main-842
```

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/telrad-au/relay/main/packaging/install.ps1))) -Version main
```

Once a stable release exists, the `releases/latest/download` installers accept
`main` too. The installer finds main builds through the anonymous GitHub API,
which is rate limited and searches only the newest 100 releases, prints the
chosen tag, and installs from that release. Each main build's installers also
install that build directly from its exact tag URL, for example
`curl -fsSL https://github.com/telrad-au/relay/releases/download/main-842-gb39dfa0/install.sh | sudo sh`.

## Protected GitHub configuration

Before creating the first stable tag:

1. Enable only squash merging for pull requests, so protected `main` remains
   linear.
2. Enable a `main` ruleset that requires a pull request, one approval,
   code-owner review when applicable, resolved conversations, linear history,
   and the Linux, Windows and primary CI checks. Block force pushes and branch
   deletion, and do not allow administrator bypass.
3. Enable a release-tag ruleset for `refs/tags/v*.*.*` that restricts tag
   creation to release maintainers and blocks tag updates, deletion and
   non-fast-forward changes.
4. Create a GitHub Environment named `production-release`. Require an
   independent reviewer, prevent self-review and restrict deployment tags to
   stable release tags. Do not allow administrator bypass where the account
   plan supports those controls.
5. Provision Azure Artifact Signing:
   - create an Artifact Signing account, complete the `Telrad Pty Ltd` public
     identity validation and create a `PublicTrust` certificate profile;
   - create a Microsoft Entra application with a GitHub federated credential
     whose subject is
     `repo:telrad-au/relay:environment:production-release` and whose audience
     is `api://AzureADTokenExchange`; and
   - grant that identity the `Artifact Signing Certificate Profile Signer` role
     at the narrowest available scope. Do not create an Azure client secret.
   Follow Microsoft's [Artifact Signing
   quickstart](https://learn.microsoft.com/en-us/azure/artifact-signing/quickstart)
   and [GitHub OIDC
   guidance](https://learn.microsoft.com/en-us/azure/developer/github/connect-from-azure-openid-connect).
6. Configure these `production-release` environment secrets:
   - `AZURE_ARTIFACT_SIGNING_CLIENT_ID`: the Entra application (client) ID;
   - `AZURE_ARTIFACT_SIGNING_TENANT_ID`: the Microsoft Entra tenant ID;
   - `AZURE_ARTIFACT_SIGNING_SUBSCRIPTION_ID`: the Azure subscription ID;
   - `AZURE_ARTIFACT_SIGNING_ENDPOINT`: the regional Artifact Signing
     endpoint, such as `https://eus.codesigning.azure.net/`;
   - `AZURE_ARTIFACT_SIGNING_ACCOUNT_NAME`: the Artifact Signing account name;
     and
   - `AZURE_ARTIFACT_SIGNING_CERTIFICATE_PROFILE_NAME`: the `PublicTrust`
     certificate profile name.
   These identifiers are not private keys, but they stay environment secrets
   so only the protected signing job receives them.
7. Enable GitHub private vulnerability reporting.
8. Enable GitHub immutable releases. This applies prospectively, so it must
   happen before the first stable release.

The stable workflow's read-only `plan` job runs before the environment gate and
records the stable tag, source revision, selected prerelease, digest and
platforms in the workflow summary. Review that summary before approving. The
protected job signs the Windows binary through GitHub OIDC and Azure Artifact
Signing, applies a Microsoft RFC 3161 timestamp and verifies Authenticode. It
cannot publish. Approving it authorises production signing and the dependent
automatic publication if every verification succeeds.

### Removed

The following protected configuration served self-update and the testing
channel, which no longer exist. An administrator can delete it:

- the `testing-release` environment, including the secret
  `RELAY_TESTING_UPDATE_SIGNING_KEY_BASE64` and the variable
  `RELAY_TESTING_UPDATE_PUBLIC_KEY_BASE64`;
- the `development-release` secrets `RELAY_DEV_UPDATE_SIGNING_KEY_BASE64` and
  `RELAY_DEV_UPDATE_PUBLIC_KEY_BASE64`;
- the `production-release` secrets `RELAY_UPDATE_SIGNING_KEY_BASE64` and
  `RELAY_UPDATE_TRUSTED_PUBLIC_KEY_BASE64`;
- the variables `RELAY_DEV_ENROLLMENT_URL` and `RELAY_ENROLLMENT_URL`, and the
  `development-release` environment itself, once no workflow references them;
- the offline Ed25519 update key pairs for the production, development and
  testing channels; and
- the mutable `testing` container tag and any `testing-*` GitHub prereleases.

## Tagging a prerelease and stable release

Record the intended source commit once and use it for both annotated tags:

```bash
git switch main
git pull --ff-only origin main
release_commit="$(git rev-parse HEAD)"
git tag --annotate v2.1.0-rc.1 "$release_commit" --message "Telrad Relay 2.1.0 release candidate 1"
git push origin refs/tags/v2.1.0-rc.1
```

Test the prerelease: install it natively from its exact-tag installer on Linux
and Windows, run the container by digest, pair each Relay and confirm
`telrad status` reaches `ready`. Fix any issue with a higher prerelease such as
`v2.1.0-rc.2`.

When that exact commit and digest are accepted, create the stable tag from the
same commit:

```bash
git fetch origin refs/tags/v2.1.0-rc.1:refs/tags/v2.1.0-rc.1
release_commit="$(git rev-parse 'v2.1.0-rc.1^{commit}')"
git tag --annotate v2.1.0 "$release_commit" --message "Telrad Relay 2.1.0"
git push origin refs/tags/v2.1.0
```

Confirm that `release_commit` matches the SHA recorded in
`container-promotion.json`. Both release workflows require annotated tags whose
commits are on `main`, and the stable workflow fails closed if the commits
differ or the prerelease publication and digest no longer verify.

Before the first public release, also confirm the repository and GHCR package
visibility, audit Git history, Actions logs, issues and package metadata for
material unsuitable for disclosure, and confirm that `LICENSE` remains the
unmodified Apache License 2.0 text and `THIRD_PARTY_NOTICES.md` covers the
distributed dependency graph. The workflows do not change visibility.

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

On Windows, confirm that `Get-AuthenticodeSignature` reports `Valid`, the
signature chains to the expected publisher, and the RFC 3161 timestamp is
present.

After publication, install through the latest-stable URLs on clean Linux
amd64, Linux arm64 and Windows amd64 hosts, pair each Relay and confirm
`telrad status` reaches `ready`. Confirm that the prerelease version tag, the
stable version tag and `latest` resolve to the recorded digest.

## Failure before publication

The stable workflow publishes the GitHub Release only after the immutable
outputs verify, and advances `latest` as its final step. If a step fails:

- do not replace an existing version tag or release asset;
- leave the GitHub Release unpublished if it is still a draft;
- if the immutable stable container tag was already created, do not move it;
  either finish the same release from the preserved workflow artifacts after
  review or abandon that version and release a new patch version; and
- do not publish the draft or move `latest` unless the entire release
  verifies.

If the GitHub Release was published and only the final `latest` step failed,
verify the recorded stable tag and digest again, then an authorised release
maintainer can finish it with:

```bash
scripts/promote-container.sh promote-latest \
  ghcr.io/telrad-au/relay vMAJOR.MINOR.PATCH sha256:DIGEST
```

## Rollback and revocation

Relay has no in-product update or rollback. An operator rolls back by
reinstalling a previous version from its exact-tag installer, or by setting the
previous container version tag or digest and recreating the container. The
data directory or volume keeps the Relay's identity and ledger across both.

Released binaries and immutable container version tags are never overwritten.
To withdraw a bad release:

1. Stop recommending the affected release and preserve its release evidence.
2. Mark the GitHub Release as affected in its notes or a security advisory. If
   continued public download is unsafe, delete the release under the incident
   decision process; never reuse its tag or version.
3. If `latest` points to the affected image, move it to the last known-good
   digest or remove it.
4. Publish a higher patch version as soon as possible. The latest-release URLs
   then advance to it.
5. Notify affected customers with the version and digest, impact, mitigation
   and fixed version.

For an Authenticode identity or certificate compromise, disable the Entra
federated credential and its Artifact Signing role, and revoke or replace the
affected certificate profile through Azure. There is no PFX or Azure client
secret to rotate.

For a GitHub Actions or GHCR compromise, disable the release workflows, revoke
relevant tokens and sessions, compare published digests with retained release
evidence, remove mutable tags, and publish a security advisory before resuming
releases.

Record who authorised each rollback or revocation, when, the affected versions
and digests, the actions taken, customer communications and the replacement
release.
