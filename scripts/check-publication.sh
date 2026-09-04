#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

required_lines=(
    'go.mod:module github.com/telrad-au/relay'
    'Dockerfile:    org.opencontainers.image.source="https://github.com/telrad-au/relay" \'
    'SECURITY.md:reporting](https://github.com/telrad-au/relay/security/advisories/new).'
    'README.md:irm https://github.com/telrad-au/relay/releases/latest/download/install.ps1 | iex'
    '.github/workflows/publish-prerelease.yml:    IMAGE: ghcr.io/telrad-au/relay'
    '.github/workflows/publish-prerelease.yml:    PACKAGE_API_URL: https://api.github.com/orgs/telrad-au/packages/container/relay'
    '.github/workflows/publish-prerelease.yml:                  release_base_url="https://github.com/$GITHUB_REPOSITORY/releases/download/$TAG"'
    '.github/workflows/publish-prerelease.yml:        environment: development-release'
    'packaging/docker-relay.json:  "pairingUrl": "https://ingest.app.telrad.com.au/v1/relay/pairing-enrollments",'
    '.github/workflows/publish-testing.yml:        environment: testing-release'
    'scripts/build-testing-release.sh:release_feed_url="https://api.github.com/repos/$REPOSITORY/releases"'
    '.github/workflows/publish-release.yml:    IMAGE: ghcr.io/telrad-au/relay'
    '.github/workflows/publish-release.yml:    PACKAGE_API_PATH: orgs/telrad-au/packages/container/relay'
    '.github/workflows/publish-release.yml:              uses: azure/login@7ddb5af1ef8758cf1353cf3b42f940aee27ba21c # v3'
    '.github/workflows/publish-release.yml:              uses: azure/artifact-signing-action@c7ab2a863ab5f9a846ddb8265964877ef296ee82 # v2'
    '.github/workflows/publish-release.yml:                  [[ "$ENROLLMENT_URL" == "$container_pairing_url" ]] || {'
    '.github/workflows/publish-release.yml:                      scripts/finalize-signed-release.sh \'
    '.github/workflows/publish-release.yml:                  update_manifest_url="https://github.com/$GITHUB_REPOSITORY/releases/latest/download/stable.json"'
    '.github/workflows/test-azure-signing.yml:    workflow_dispatch:'
    '.github/workflows/test-azure-signing.yml:        environment: artifact-signing-test'
    '.github/workflows/test-azure-signing.yml:              uses: azure/login@7ddb5af1ef8758cf1353cf3b42f940aee27ba21c # v3'
    '.github/workflows/test-azure-signing.yml:              uses: azure/artifact-signing-action@c7ab2a863ab5f9a846ddb8265964877ef296ee82 # v2'
    '.github/workflows/test-azure-signing.yml:                  ./scripts/verify-windows-signature.ps1 `'
)

for requirement in "${required_lines[@]}"; do
    file="${requirement%%:*}"
    line="${requirement#*:}"
    grep -Fqx "$line" "$file" || {
        echo "Missing canonical publication identity in $file: $line" >&2
        exit 1
    }
done

legacy_owner='SEAN-7'
legacy_owner_lower='sean-7'
if git grep -n -E "${legacy_owner}/|${legacy_owner_lower}/|users/${legacy_owner}/packages/container/"; then
    echo "Legacy repository-owner or package-owner references remain." >&2
    exit 1
fi

if git grep -n -I -E 'RELAY_DEV_RELEASE_BASE_URL|RELAY_UPDATE_MANIFEST_URL|RELAY_R2_' -- . ':!scripts/check-publication.sh'; then
    echo "External release-host configuration remains; GitHub Releases must be the only native artifact origin." >&2
    exit 1
fi

if grep -n -E 'RELAY_WINDOWS_SIGNING_PFX_BASE64|RELAY_WINDOWS_SIGNING_PASSWORD|RELAY_WINDOWS_TIMESTAMP_URL|azure-client-secret|osslsigncode' \
    .github/workflows/publish-release.yml \
    .github/workflows/test-azure-signing.yml; then
    echo "Production signing workflow references exportable Windows signing material or the legacy signer." >&2
    exit 1
fi

if git grep -n -I -E 'BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|/Users/[^/]+/|C:\\Users\\' -- . ':!scripts/check-publication.sh'; then
    echo "Potential secret or private workstation path found in tracked content." >&2
    exit 1
fi

echo "Publication identity and tracked-content audit passed."
