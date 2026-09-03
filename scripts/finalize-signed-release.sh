#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:?usage: scripts/finalize-signed-release.sh <version> <artifact-base-url> <update-manifest-url> <pairing-url>}"
ARTIFACT_BASE_URL="${2:?usage: scripts/finalize-signed-release.sh <version> <artifact-base-url> <update-manifest-url> <pairing-url>}"
UPDATE_MANIFEST_URL="${3:?usage: scripts/finalize-signed-release.sh <version> <artifact-base-url> <update-manifest-url> <pairing-url>}"
ENROLLMENT_URL="${4:?usage: scripts/finalize-signed-release.sh <version> <artifact-base-url> <update-manifest-url> <pairing-url>}"
SIGNING_KEY="${RELAY_UPDATE_SIGNING_KEY:?RELAY_UPDATE_SIGNING_KEY must point to an Ed25519 private key}"
TRUSTED_PUBLIC_KEY="${RELAY_UPDATE_TRUSTED_PUBLIC_KEY:?RELAY_UPDATE_TRUSTED_PUBLIC_KEY must point to the pinned Ed25519 public key}"
WINDOWS_SIGNER_CERTIFICATE_SHA256_FILE="${RELAY_WINDOWS_SIGNER_CERTIFICATE_SHA256_FILE:?RELAY_WINDOWS_SIGNER_CERTIFICATE_SHA256_FILE must contain the verified Authenticode signer certificate pin}"
RELEASE_TAG="${RELAY_RELEASE_TAG:-v${VERSION}}"
SOURCE_REVISION="${RELAY_SOURCE_REVISION:-$(git -C "$ROOT_DIR" rev-parse HEAD)}"
OUTPUT_DIR="${RELAY_RELEASE_DIR:-${ROOT_DIR}/dist/${VERSION}}"
WINDOWS_SIGNING_CA_FILE="${RELAY_WINDOWS_SIGNING_CA_FILE:-}"

[[ "$OUTPUT_DIR" == /* ]] || OUTPUT_DIR="${PWD}/${OUTPUT_DIR}"
[[ -d "$OUTPUT_DIR" ]] || {
    echo "Prebuilt release directory does not exist: $OUTPUT_DIR" >&2
    exit 1
}
[[ -f "$SIGNING_KEY" ]] || {
    echo "Signing key does not exist: $SIGNING_KEY" >&2
    exit 1
}
for required_file in "$TRUSTED_PUBLIC_KEY" "$WINDOWS_SIGNER_CERTIFICATE_SHA256_FILE"; do
    [[ -f "$required_file" ]] || {
        echo "Required signing file does not exist: $required_file" >&2
        exit 1
    }
done
[[ -z "$WINDOWS_SIGNING_CA_FILE" || -f "$WINDOWS_SIGNING_CA_FILE" ]] || {
    echo "Windows signing CA file does not exist: $WINDOWS_SIGNING_CA_FILE" >&2
    exit 1
}
[[ "$RELEASE_TAG" == "v$VERSION" ]] || {
    echo "Release tag must be v$VERSION." >&2
    exit 1
}
[[ "$SOURCE_REVISION" =~ ^[0-9a-f]{40}$ ]] || {
    echo "Source revision must be a 40-character lowercase Git commit." >&2
    exit 1
}
for command in base64 jq openssl sha256sum; do
    command -v "$command" >/dev/null 2>&1 || {
        echo "Missing required command: $command" >&2
        exit 1
    }
done
for address in "$ARTIFACT_BASE_URL" "$UPDATE_MANIFEST_URL" "$ENROLLMENT_URL"; do
    [[ "$address" == https://* ]] || {
        echo "Production release URLs must use HTTPS: $address" >&2
        exit 1
    }
    [[ "$address" != *"|"* && "$address" != *"&"* && "$address" != *'"'* && "$address" != *'$'* && "$address" != *'`'* && "$address" != *"\\"* && "$address" != *$'\r'* && "$address" != *$'\n'* ]] || {
        echo "Production release URLs contain unsupported characters." >&2
        exit 1
    }
done
[[ "$ENROLLMENT_URL" == https://*/v1/relay/pairing-enrollments ]] || {
    echo "Production Relay pairing URL must use /v1/relay/pairing-enrollments." >&2
    exit 1
}

LINUX_BINARY="$OUTPUT_DIR/telrad-relay-linux-amd64"
LINUX_ARM64_BINARY="$OUTPUT_DIR/telrad-relay-linux-arm64"
WINDOWS_BINARY="$OUTPUT_DIR/telrad-relay-windows-amd64.exe"
for required_artifact in "$LINUX_BINARY" "$LINUX_ARM64_BINARY" "$WINDOWS_BINARY"; do
    [[ -f "$required_artifact" ]] || {
        echo "Prebuilt release artifact does not exist: $required_artifact" >&2
        exit 1
    }
done

derived_update_key_sha256="$(openssl pkey -in "$SIGNING_KEY" -pubout -outform DER | sha256sum | awk '{print $1}')"
trusted_update_key_sha256="$(openssl pkey -pubin -in "$TRUSTED_PUBLIC_KEY" -outform DER | sha256sum | awk '{print $1}')"
[[ "$derived_update_key_sha256" == "$trusted_update_key_sha256" ]] || {
    echo "Update signing key does not match the pinned public key." >&2
    exit 1
}

WINDOWS_SIGNER_CERTIFICATE_SHA256="$(tr -d '\r\n' < "$WINDOWS_SIGNER_CERTIFICATE_SHA256_FILE")"
[[ "$WINDOWS_SIGNER_CERTIFICATE_SHA256" =~ ^[0-9a-f]{64}$ ]] || {
    echo "Windows signer certificate pin is not a canonical SHA-256 digest." >&2
    exit 1
}
if command -v osslsigncode >/dev/null 2>&1; then
    WINDOWS_VERIFY_ARGS=(-require-leaf-hash "sha256:$WINDOWS_SIGNER_CERTIFICATE_SHA256")
    if [[ -n "$WINDOWS_SIGNING_CA_FILE" ]]; then
        WINDOWS_VERIFY_ARGS+=(-CAfile "$WINDOWS_SIGNING_CA_FILE")
    fi
    osslsigncode verify "${WINDOWS_VERIFY_ARGS[@]}" -in "$WINDOWS_BINARY"
elif [[ "${RELAY_WINDOWS_SIGNATURE_VERIFIED:-false}" != "true" ]]; then
    echo "The caller must verify Authenticode and set RELAY_WINDOWS_SIGNATURE_VERIFIED=true when osslsigncode is unavailable." >&2
    exit 1
fi

verify_generated_bundle() {
    local verify_dir="$OUTPUT_DIR"
    if command -v cygpath >/dev/null 2>&1; then
        verify_dir="$(cygpath -w "$OUTPUT_DIR")"
    fi
    if command -v go >/dev/null 2>&1; then
        (
            cd "$ROOT_DIR"
            RELAY_RELEASE_VERIFY_DIR="$verify_dir" \
                go test ./cmd/telrad-relay -run '^TestGeneratedReleaseBundle$' -count=1
        )
    else
        docker run --rm \
            --user "$(id -u):$(id -g)" \
            -e RELAY_RELEASE_VERIFY_DIR=/release \
            -e GOCACHE=/tmp/go-cache \
            -e GOMODCACHE=/tmp/go-mod-cache \
            -v "${ROOT_DIR}:/workspace:ro" \
            -v "${OUTPUT_DIR}:/release:ro" \
            -w /workspace \
            golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc \
            sh -c "go test ./cmd/telrad-relay -run '^TestGeneratedReleaseBundle$' -count=1"
    fi
}

sign_digest() {
    local artifact="$1"
    openssl pkeyutl -sign -rawin -inkey "$SIGNING_KEY" \
        -in "${artifact}.sha256" -out "${artifact}.sig"
}

sign_update_statement() {
    local platform="$1"
    local artifact="$2"
    local artifact_url="$3"
    local digest
    digest="$(< "${artifact}.sha256")"
    local statement="${artifact}.update-statement"
    printf 'telrad-relay-update-v2\nchannel=stable\nversion=%s\nreleaseTag=%s\nsourceRevision=%s\nplatform=%s\nurl=%s\nsha256=%s\n' \
        "$VERSION" "$RELEASE_TAG" "$SOURCE_REVISION" "$platform" "$artifact_url" "$digest" > "$statement"
    openssl pkeyutl -sign -rawin -inkey "$SIGNING_KEY" \
        -in "$statement" -out "${artifact}.update.sig"
    rm "$statement"
}

encode_base64() {
    base64 < "$1" | tr -d '\r\n'
}

# Authenticode changes the PE bytes, so replace the unsigned build digest before
# creating either detached Ed25519 signature.
printf '%s' "$(sha256sum "$WINDOWS_BINARY" | awk '{print tolower($1)}')" > "${WINDOWS_BINARY}.sha256"
sign_digest "$LINUX_BINARY"
sign_digest "$LINUX_ARM64_BINARY"
sign_digest "$WINDOWS_BINARY"

LINUX_URL="${ARTIFACT_BASE_URL%/}/$(basename "$LINUX_BINARY")"
LINUX_ARM64_URL="${ARTIFACT_BASE_URL%/}/$(basename "$LINUX_ARM64_BINARY")"
WINDOWS_URL="${ARTIFACT_BASE_URL%/}/$(basename "$WINDOWS_BINARY")"
sign_update_statement linux-amd64 "$LINUX_BINARY" "$LINUX_URL"
sign_update_statement linux-arm64 "$LINUX_ARM64_BINARY" "$LINUX_ARM64_URL"
sign_update_statement windows-amd64 "$WINDOWS_BINARY" "$WINDOWS_URL"

cp "$TRUSTED_PUBLIC_KEY" "$OUTPUT_DIR/update-public-key.pem"
chmod 0644 "$OUTPUT_DIR/update-public-key.pem"
openssl pkey -pubin -in "$TRUSTED_PUBLIC_KEY" -outform DER \
    | tail -c 32 \
    | base64 \
    | tr -d '\r\n' > "$OUTPUT_DIR/update-public-key.txt"
printf '\n' >> "$OUTPUT_DIR/update-public-key.txt"

LINUX_SHA="$(tr -d '\r\n ' < "${LINUX_BINARY}.sha256")"
LINUX_ARM64_SHA="$(tr -d '\r\n ' < "${LINUX_ARM64_BINARY}.sha256")"
WINDOWS_SHA="$(tr -d '\r\n ' < "${WINDOWS_BINARY}.sha256")"
LINUX_SIGNATURE="$(encode_base64 "${LINUX_BINARY}.update.sig")"
LINUX_ARM64_SIGNATURE="$(encode_base64 "${LINUX_ARM64_BINARY}.update.sig")"
WINDOWS_SIGNATURE="$(encode_base64 "${WINDOWS_BINARY}.update.sig")"
UPDATE_PUBLIC_KEY_PEM_BASE64="$(encode_base64 "$OUTPUT_DIR/update-public-key.pem")"
SERVICE_FILE_BASE64="$(encode_base64 "$OUTPUT_DIR/telrad-relay.service")"
INSTALLATION_MANIFEST_BASE64="$(encode_base64 "$OUTPUT_DIR/installation-manifest.json")"

jq -n \
    --argjson schemaVersion 2 \
    --arg channel stable \
    --arg version "$VERSION" \
    --arg releaseTag "$RELEASE_TAG" \
    --arg sourceRevision "$SOURCE_REVISION" \
    --arg linuxUrl "$LINUX_URL" \
    --arg linuxSha "$LINUX_SHA" \
    --arg linuxSignature "$LINUX_SIGNATURE" \
    --arg linuxArm64Url "$LINUX_ARM64_URL" \
    --arg linuxArm64Sha "$LINUX_ARM64_SHA" \
    --arg linuxArm64Signature "$LINUX_ARM64_SIGNATURE" \
    --arg windowsUrl "$WINDOWS_URL" \
    --arg windowsSha "$WINDOWS_SHA" \
    --arg windowsSignature "$WINDOWS_SIGNATURE" \
    '{schemaVersion:$schemaVersion,channel:$channel,version:$version,releaseTag:$releaseTag,sourceRevision:$sourceRevision,artifacts:{"linux-amd64":{url:$linuxUrl,sha256:$linuxSha,signature:$linuxSignature},"linux-arm64":{url:$linuxArm64Url,sha256:$linuxArm64Sha,signature:$linuxArm64Signature},"windows-amd64":{url:$windowsUrl,sha256:$windowsSha,signature:$windowsSignature}}}' \
    > "$OUTPUT_DIR/stable.json"

sed -e "s|@@ARTIFACT_BASE_URL@@|$ARTIFACT_BASE_URL|g" \
    -e "s|@@UPDATE_MANIFEST_URL@@|$UPDATE_MANIFEST_URL|g" \
    -e "s|@@ENROLLMENT_URL@@|$ENROLLMENT_URL|g" \
    -e "s|@@UPDATE_PUBLIC_KEY@@|$(< "$OUTPUT_DIR/update-public-key.txt")|g" \
    -e "s|@@UPDATE_PUBLIC_KEY_PEM_BASE64@@|$UPDATE_PUBLIC_KEY_PEM_BASE64|g" \
    -e "s|@@SERVICE_FILE_BASE64@@|$SERVICE_FILE_BASE64|g" \
    -e "s|@@INSTALLATION_MANIFEST_BASE64@@|$INSTALLATION_MANIFEST_BASE64|g" \
    "$ROOT_DIR/packaging/install-hosted.sh.template" > "$OUTPUT_DIR/install.sh"
sed -e "s|@@ARTIFACT_BASE_URL@@|$ARTIFACT_BASE_URL|g" \
    -e "s|@@UPDATE_MANIFEST_URL@@|$UPDATE_MANIFEST_URL|g" \
    -e "s|@@ENROLLMENT_URL@@|$ENROLLMENT_URL|g" \
    -e "s|@@UPDATE_PUBLIC_KEY@@|$(< "$OUTPUT_DIR/update-public-key.txt")|g" \
    -e "s|@@WINDOWS_SIGNER_CERTIFICATE_SHA256@@|$WINDOWS_SIGNER_CERTIFICATE_SHA256|g" \
    -e "s|@@INSTALLATION_MANIFEST_BASE64@@|$INSTALLATION_MANIFEST_BASE64|g" \
    "$ROOT_DIR/packaging/install-hosted.ps1.template" > "$OUTPUT_DIR/install.ps1"
printf '%s\n' "$WINDOWS_SIGNER_CERTIFICATE_SHA256" > "$OUTPUT_DIR/windows-signer-certificate-sha256.txt"
chmod 0755 "$OUTPUT_DIR/install.sh" "$LINUX_BINARY" "$LINUX_ARM64_BINARY"

for release_artifact in "linux-amd64:$LINUX_BINARY" "linux-arm64:$LINUX_ARM64_BINARY" "windows-amd64:$WINDOWS_BINARY"; do
    platform="${release_artifact%%:*}"
    artifact="${release_artifact#*:}"
    expected="$(< "${artifact}.sha256")"
    [[ "$expected" =~ ^[0-9a-f]{64}$ && "$(wc -c < "${artifact}.sha256" | tr -d ' ')" == "64" ]] || {
        echo "Digest is not canonical for $(basename "$artifact")" >&2
        exit 1
    }
    actual="$(sha256sum "$artifact" | awk '{print tolower($1)}')"
    [[ "$actual" == "$expected" ]] || {
        echo "Digest verification failed for $(basename "$artifact")" >&2
        exit 1
    }
    manifest_digest="$(jq --exit-status --raw-output --arg platform "$platform" '.artifacts[$platform].sha256' "$OUTPUT_DIR/stable.json")"
    [[ "$manifest_digest" == "$expected" ]] || {
        echo "Manifest digest does not match $(basename "${artifact}.sha256")" >&2
        exit 1
    }
    manifest_signature="${artifact}.update.sig"
    manifest_statement="${artifact}.manifest-statement"
    manifest_url="$(jq --exit-status --raw-output --arg platform "$platform" '.artifacts[$platform].url' "$OUTPUT_DIR/stable.json")"
    printf 'telrad-relay-update-v2\nchannel=stable\nversion=%s\nreleaseTag=%s\nsourceRevision=%s\nplatform=%s\nurl=%s\nsha256=%s\n' \
        "$VERSION" "$RELEASE_TAG" "$SOURCE_REVISION" "$platform" "$manifest_url" "$manifest_digest" > "$manifest_statement"
    if ! openssl pkeyutl -verify -pubin -inkey "$OUTPUT_DIR/update-public-key.pem" -rawin \
        -in "$manifest_statement" -sigfile "$manifest_signature" >/dev/null; then
        rm -f "$manifest_statement"
        echo "Manifest signature verification failed for $(basename "$artifact")" >&2
        exit 1
    fi
    rm -f "$manifest_statement"
done

verify_generated_bundle

printf 'Signed release bundle written to %s\n' "$OUTPUT_DIR"
