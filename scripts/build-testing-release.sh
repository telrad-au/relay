#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:?usage: scripts/build-testing-release.sh <version> <tag> <repository> <source-revision>}"
TAG="${2:?usage: scripts/build-testing-release.sh <version> <tag> <repository> <source-revision>}"
REPOSITORY="${3:?usage: scripts/build-testing-release.sh <version> <tag> <repository> <source-revision>}"
SOURCE_REVISION="${4:?usage: scripts/build-testing-release.sh <version> <tag> <repository> <source-revision>}"
SIGNING_KEY="${RELAY_TESTING_UPDATE_SIGNING_KEY:?RELAY_TESTING_UPDATE_SIGNING_KEY must point to the testing Ed25519 private key}"
TRUSTED_PUBLIC_KEY="${RELAY_TESTING_UPDATE_PUBLIC_KEY:?RELAY_TESTING_UPDATE_PUBLIC_KEY must point to the testing Ed25519 public key}"
RELEASE_DIR="${RELAY_RELEASE_DIR:-${ROOT_DIR}/dist/${VERSION}}"
ASSET_DIR="${RELAY_TESTING_ASSET_DIR:-${ROOT_DIR}/dist/testing-assets}"
[[ "$RELEASE_DIR" == /* ]] || RELEASE_DIR="${PWD}/${RELEASE_DIR}"
[[ "$ASSET_DIR" == /* ]] || ASSET_DIR="${PWD}/${ASSET_DIR}"

[[ "$VERSION" =~ ^0\.0\.0-testing\.([0-9]+)\.([0-9]+)$ ]] || {
    echo "Testing version must use 0.0.0-testing.RUN.ATTEMPT." >&2
    exit 1
}
expected_tag="testing-${BASH_REMATCH[1]}-${BASH_REMATCH[2]}"
[[ "$TAG" == "$expected_tag" ]] || {
    echo "Testing tag $TAG does not match version $VERSION." >&2
    exit 1
}
[[ "$REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || {
    echo "Testing repository is invalid." >&2
    exit 1
}
[[ "$SOURCE_REVISION" =~ ^[0-9a-f]{40}$ ]] || {
    echo "Testing source revision must be a 40-character lowercase Git commit." >&2
    exit 1
}
for required_file in "$SIGNING_KEY" "$TRUSTED_PUBLIC_KEY"; do
    [[ -f "$required_file" ]] || {
        echo "Required testing signing file does not exist: $required_file" >&2
        exit 1
    }
done
for command in base64 jq openssl sha256sum; do
    command -v "$command" >/dev/null 2>&1 || {
        echo "Missing required command: $command" >&2
        exit 1
    }
done

derived_key_sha256="$(openssl pkey -in "$SIGNING_KEY" -pubout -outform DER | sha256sum | awk '{print $1}')"
trusted_key_sha256="$(openssl pkey -pubin -in "$TRUSTED_PUBLIC_KEY" -outform DER | sha256sum | awk '{print $1}')"
[[ "$derived_key_sha256" == "$trusted_key_sha256" ]] || {
    echo "Testing update signing key does not match the pinned public key." >&2
    exit 1
}

mkdir -p "$ASSET_DIR"
artifact_base_url="https://github.com/$REPOSITORY/releases/download/$TAG"
release_feed_url="https://api.github.com/repos/$REPOSITORY/releases"
encoded_public_key="$(openssl pkey -pubin -in "$TRUSTED_PUBLIC_KEY" -outform DER | tail -c 32 | base64 -w0)"

sign_artifact() {
    local platform="$1"
    local filename="$2"
    local source="$RELEASE_DIR/$filename"
    local destination="$ASSET_DIR/$filename"
    [[ -f "$source" && -f "$source.sha256" ]] || {
        echo "Testing release artifact is missing: $source" >&2
        exit 1
    }
    install -m 0755 "$source" "$destination"
    install -m 0644 "$source.sha256" "$destination.sha256"
    local digest
    digest="$(< "$destination.sha256")"
    [[ "$digest" =~ ^[0-9a-f]{64}$ ]] || {
        echo "Testing release digest is malformed: $destination.sha256" >&2
        exit 1
    }
    [[ "$(sha256sum "$destination" | awk '{print tolower($1)}')" == "$digest" ]] || {
        echo "Testing release digest does not match $filename." >&2
        exit 1
    }
    local artifact_url="$artifact_base_url/$filename"
    local statement="$ASSET_DIR/$filename.update-statement"
    printf 'telrad-relay-update-v2\nchannel=testing\nversion=%s\nreleaseTag=%s\nsourceRevision=%s\nplatform=%s\nurl=%s\nsha256=%s\n' \
        "$VERSION" "$TAG" "$SOURCE_REVISION" "$platform" "$artifact_url" "$digest" > "$statement"
    openssl pkeyutl -sign -rawin -inkey "$SIGNING_KEY" \
        -in "$statement" -out "$destination.update.sig"
    rm "$statement"
}

sign_artifact linux-amd64 telrad-relay-linux-amd64
sign_artifact linux-arm64 telrad-relay-linux-arm64
sign_artifact windows-amd64 telrad-relay-windows-amd64.exe

linux_sha="$(< "$ASSET_DIR/telrad-relay-linux-amd64.sha256")"
linux_arm64_sha="$(< "$ASSET_DIR/telrad-relay-linux-arm64.sha256")"
windows_sha="$(< "$ASSET_DIR/telrad-relay-windows-amd64.exe.sha256")"
linux_signature="$(base64 -w0 < "$ASSET_DIR/telrad-relay-linux-amd64.update.sig")"
linux_arm64_signature="$(base64 -w0 < "$ASSET_DIR/telrad-relay-linux-arm64.update.sig")"
windows_signature="$(base64 -w0 < "$ASSET_DIR/telrad-relay-windows-amd64.exe.update.sig")"

jq -n \
    --argjson schemaVersion 2 \
    --arg channel testing \
    --arg version "$VERSION" \
    --arg releaseTag "$TAG" \
    --arg sourceRevision "$SOURCE_REVISION" \
    --arg linuxUrl "$artifact_base_url/telrad-relay-linux-amd64" \
    --arg linuxSha "$linux_sha" \
    --arg linuxSignature "$linux_signature" \
    --arg linuxArm64Url "$artifact_base_url/telrad-relay-linux-arm64" \
    --arg linuxArm64Sha "$linux_arm64_sha" \
    --arg linuxArm64Signature "$linux_arm64_signature" \
    --arg windowsUrl "$artifact_base_url/telrad-relay-windows-amd64.exe" \
    --arg windowsSha "$windows_sha" \
    --arg windowsSignature "$windows_signature" \
    '{schemaVersion:$schemaVersion,channel:$channel,version:$version,releaseTag:$releaseTag,sourceRevision:$sourceRevision,artifacts:{"linux-amd64":{url:$linuxUrl,sha256:$linuxSha,signature:$linuxSignature},"linux-arm64":{url:$linuxArm64Url,sha256:$linuxArm64Sha,signature:$linuxArm64Signature},"windows-amd64":{url:$windowsUrl,sha256:$windowsSha,signature:$windowsSignature}}}' \
    > "$ASSET_DIR/testing.json"

jq -n \
    --argjson schemaVersion 1 \
    --arg channel testing \
    --arg releaseFeedUrl "$release_feed_url" \
    --arg publicKey "$encoded_public_key" \
    '{schemaVersion:$schemaVersion,channel:$channel,releaseFeedUrl:$releaseFeedUrl,publicKey:$publicKey}' \
    > "$ASSET_DIR/update-trust.json"

if command -v go >/dev/null 2>&1; then
    (
        cd "$ROOT_DIR"
        RELAY_TESTING_RELEASE_VERIFY_DIR="$ASSET_DIR" \
            go test ./cmd/telrad-relay -run '^TestGeneratedTestingReleaseBundle$' -count=1
    )
fi

printf 'Signed testing release metadata written to %s\n' "$ASSET_DIR"
