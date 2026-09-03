#!/usr/bin/env bash
set -euo pipefail

# This wrapper keeps the exportable-PKCS#12 path for local and CI contract
# tests. Production builds use Azure Artifact Signing and call
# finalize-signed-release.sh with an already signed Windows executable.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:?usage: scripts/build-signed-release.sh <version> <artifact-base-url> <update-manifest-url> <pairing-url>}"
ARTIFACT_BASE_URL="${2:?usage: scripts/build-signed-release.sh <version> <artifact-base-url> <update-manifest-url> <pairing-url>}"
UPDATE_MANIFEST_URL="${3:?usage: scripts/build-signed-release.sh <version> <artifact-base-url> <update-manifest-url> <pairing-url>}"
ENROLLMENT_URL="${4:?usage: scripts/build-signed-release.sh <version> <artifact-base-url> <update-manifest-url> <pairing-url>}"
WINDOWS_SIGNING_PFX="${RELAY_WINDOWS_SIGNING_PFX:?RELAY_WINDOWS_SIGNING_PFX must point to the Authenticode PKCS#12 file}"
WINDOWS_SIGNING_PASSWORD_FILE="${RELAY_WINDOWS_SIGNING_PASSWORD_FILE:?RELAY_WINDOWS_SIGNING_PASSWORD_FILE must contain the PKCS#12 password}"
WINDOWS_TIMESTAMP_URL="${RELAY_WINDOWS_TIMESTAMP_URL:-}"
ALLOW_UNTIMESTAMPED_WINDOWS="${RELAY_WINDOWS_ALLOW_UNTIMESTAMPED:-false}"
WINDOWS_SIGNING_CA_FILE="${RELAY_WINDOWS_SIGNING_CA_FILE:-}"
RELEASE_TAG="${RELAY_RELEASE_TAG:-v${VERSION}}"
SOURCE_REVISION="${RELAY_SOURCE_REVISION:-$(git -C "$ROOT_DIR" rev-parse HEAD)}"
OUTPUT_DIR="${RELAY_RELEASE_DIR:-${ROOT_DIR}/dist/${VERSION}}"

[[ "$OUTPUT_DIR" == /* ]] || OUTPUT_DIR="${PWD}/${OUTPUT_DIR}"
[[ "$RELEASE_TAG" == "v$VERSION" ]] || {
    echo "Release tag must be v$VERSION." >&2
    exit 1
}
[[ "$SOURCE_REVISION" =~ ^[0-9a-f]{40}$ ]] || {
    echo "Source revision must be a 40-character lowercase Git commit." >&2
    exit 1
}
for required_file in "$WINDOWS_SIGNING_PFX" "$WINDOWS_SIGNING_PASSWORD_FILE"; do
    [[ -f "$required_file" ]] || {
        echo "Required signing file does not exist: $required_file" >&2
        exit 1
    }
done
[[ -z "$WINDOWS_SIGNING_CA_FILE" || -f "$WINDOWS_SIGNING_CA_FILE" ]] || {
    echo "Windows signing CA file does not exist: $WINDOWS_SIGNING_CA_FILE" >&2
    exit 1
}
for command in openssl osslsigncode sha256sum; do
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
done
[[ -n "$WINDOWS_TIMESTAMP_URL" || ( "$ALLOW_UNTIMESTAMPED_WINDOWS" == "true" && "$VERSION" == *-ci.* ) ]] || {
    echo "RELAY_WINDOWS_TIMESTAMP_URL must be an RFC 3161 timestamp URL." >&2
    exit 1
}

RELAY_RELEASE_DIR="$OUTPUT_DIR" "$ROOT_DIR/scripts/build-release.sh" "$VERSION"

WINDOWS_BINARY="$OUTPUT_DIR/telrad-relay-windows-amd64.exe"
UNSIGNED_WINDOWS_BINARY="${WINDOWS_BINARY}.unsigned"
mv "$WINDOWS_BINARY" "$UNSIGNED_WINDOWS_BINARY"
WINDOWS_SIGN_ARGS=(
    -pkcs12 "$WINDOWS_SIGNING_PFX"
    -readpass "$WINDOWS_SIGNING_PASSWORD_FILE"
    -h sha256
    -n "Telrad Relay"
)
if [[ -n "$WINDOWS_TIMESTAMP_URL" ]]; then
    WINDOWS_SIGN_ARGS+=(-ts "$WINDOWS_TIMESTAMP_URL")
else
    echo "WARNING: creating an untimestamped Windows signature for test verification only." >&2
fi
if ! osslsigncode sign \
    "${WINDOWS_SIGN_ARGS[@]}" \
    -in "$UNSIGNED_WINDOWS_BINARY" \
    -out "$WINDOWS_BINARY"; then
    mv "$UNSIGNED_WINDOWS_BINARY" "$WINDOWS_BINARY"
    exit 1
fi
rm "$UNSIGNED_WINDOWS_BINARY"

WINDOWS_SIGNER_CERTIFICATE_SHA256="$(
    openssl pkcs12 -in "$WINDOWS_SIGNING_PFX" -clcerts -nokeys \
        -passin "file:$WINDOWS_SIGNING_PASSWORD_FILE" \
        | openssl x509 -outform DER \
        | sha256sum \
        | awk '{print $1}'
)"
[[ "$WINDOWS_SIGNER_CERTIFICATE_SHA256" =~ ^[0-9a-f]{64}$ ]] || {
    echo "Could not derive the Windows signer certificate pin." >&2
    exit 1
}
WINDOWS_VERIFY_ARGS=(-require-leaf-hash "sha256:$WINDOWS_SIGNER_CERTIFICATE_SHA256")
if [[ -n "$WINDOWS_SIGNING_CA_FILE" ]]; then
    WINDOWS_VERIFY_ARGS+=(-CAfile "$WINDOWS_SIGNING_CA_FILE")
fi
osslsigncode verify "${WINDOWS_VERIFY_ARGS[@]}" -in "$WINDOWS_BINARY"

signer_pin_file="$OUTPUT_DIR/windows-signer-certificate-sha256.txt"
printf '%s\n' "$WINDOWS_SIGNER_CERTIFICATE_SHA256" > "$signer_pin_file"
RELAY_RELEASE_DIR="$OUTPUT_DIR" \
RELAY_WINDOWS_SIGNER_CERTIFICATE_SHA256_FILE="$signer_pin_file" \
    "$ROOT_DIR/scripts/finalize-signed-release.sh" \
    "$VERSION" \
    "$ARTIFACT_BASE_URL" \
    "$UPDATE_MANIFEST_URL" \
    "$ENROLLMENT_URL"
