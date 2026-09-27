#!/usr/bin/env bash
# Builds the native release directory: Linux amd64/arm64 and Windows amd64
# binaries, the systemd unit, both installers, licences, per-binary .sha256
# files and SHA256SUMS. RELAY_ENROLMENT_URL optionally replaces the built-in
# production enrolment endpoint (prereleases and tests only).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:?usage: scripts/build-release.sh <version>}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] || {
    echo "Version must be SemVer without a leading v or build metadata (for example 2.1.0-rc.1)." >&2
    exit 1
}
ENROLMENT_URL="${RELAY_ENROLMENT_URL:-}"
[[ -z "$ENROLMENT_URL" || "$ENROLMENT_URL" =~ ^https://[A-Za-z0-9.:-]+/[A-Za-z0-9/._-]*$ ]] || {
    echo "RELAY_ENROLMENT_URL must be a plain https URL." >&2
    exit 1
}

OUTPUT_DIR="${RELAY_RELEASE_DIR:-${ROOT_DIR}/dist/${VERSION}}"
[[ "$OUTPUT_DIR" == /* ]] || OUTPUT_DIR="${PWD}/${OUTPUT_DIR}"
mkdir -p "$OUTPUT_DIR"

LDFLAGS="-s -w -X main.version=${VERSION}"
[[ -z "$ENROLMENT_URL" ]] || LDFLAGS="${LDFLAGS} -X main.defaultEnrolmentURL=${ENROLMENT_URL}"

build_binary() {
    local goos="$1"
    local goarch="$2"
    local output="$3"

    if command -v go >/dev/null 2>&1; then
        (
            cd "$ROOT_DIR"
            GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
                go build -trimpath -ldflags="$LDFLAGS" -o "$OUTPUT_DIR/$output" ./cmd/telrad-relay
        )
    else
        docker run --rm \
            --user "$(id -u):$(id -g)" \
            -e GOOS="$goos" \
            -e GOARCH="$goarch" \
            -e CGO_ENABLED=0 \
            -e GOCACHE=/tmp/go-cache \
            -e GOMODCACHE=/tmp/go-mod-cache \
            -e LDFLAGS="$LDFLAGS" \
            -e OUTPUT_NAME="$output" \
            -v "${ROOT_DIR}:/workspace:ro" \
            -v "${OUTPUT_DIR}:/out" \
            -w /workspace \
            golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc \
            sh -c 'go build -trimpath -ldflags="$LDFLAGS" -o "/out/$OUTPUT_NAME" ./cmd/telrad-relay'
    fi
}

build_binary linux amd64 telrad-relay-linux-amd64
build_binary linux arm64 telrad-relay-linux-arm64
build_binary windows amd64 telrad-relay-windows-amd64.exe
install -m 0644 "$ROOT_DIR/cmd/telrad-relay/telrad-relay.service" "$OUTPUT_DIR/telrad-relay.service"
install -m 0755 "$ROOT_DIR/packaging/install.sh" "$OUTPUT_DIR/install.sh"
install -m 0644 "$ROOT_DIR/packaging/install.ps1" "$OUTPUT_DIR/install.ps1"
install -m 0644 "$ROOT_DIR/LICENSE" "$OUTPUT_DIR/LICENSE"
install -m 0644 "$ROOT_DIR/NOTICE" "$OUTPUT_DIR/NOTICE"
install -m 0644 "$ROOT_DIR/THIRD_PARTY_NOTICES.md" "$OUTPUT_DIR/THIRD_PARTY_NOTICES.md"
"$ROOT_DIR/scripts/write-checksums.sh" "$OUTPUT_DIR"

printf 'Release artifacts written to %s\n' "$OUTPUT_DIR"
