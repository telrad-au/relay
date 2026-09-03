#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:?usage: scripts/build-release.sh <version>}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] || {
    echo "Version must be SemVer without a leading v or build metadata (for example 2.1.0-rc.1)." >&2
    exit 1
}

OUTPUT_DIR="${RELAY_RELEASE_DIR:-${ROOT_DIR}/dist/${VERSION}}"
[[ "$OUTPUT_DIR" == /* ]] || OUTPUT_DIR="${PWD}/${OUTPUT_DIR}"
mkdir -p "$OUTPUT_DIR"

build_binary() {
    local goos="$1"
    local goarch="$2"
    local suffix="$3"
    local output="${OUTPUT_DIR}/telrad-relay-${goos}-${goarch}${suffix}"

    if command -v go >/dev/null 2>&1; then
        (
            cd "$ROOT_DIR"
            GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
                go build -trimpath \
                -ldflags="-s -w -X main.version=${VERSION}" \
                -o "$output" ./cmd/telrad-relay
        )
    else
        docker run --rm \
            --user "$(id -u):$(id -g)" \
            -e GOOS="$goos" \
            -e GOARCH="$goarch" \
            -e GOCACHE=/tmp/go-cache \
            -e GOMODCACHE=/tmp/go-mod-cache \
            -e VERSION="$VERSION" \
            -e OUTPUT_NAME="$(basename "$output")" \
            -v "${ROOT_DIR}:/workspace:ro" \
            -v "${OUTPUT_DIR}:/out" \
            -w /workspace \
            golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc \
            sh -c 'CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o "/out/${OUTPUT_NAME}" ./cmd/telrad-relay'
    fi

    [[ -f "$output" ]] || {
        echo "Build did not produce $output" >&2
        return 1
    }
    local digest
    digest="$(sha256sum "$output" | awk '{print tolower($1)}')"
    [[ "$digest" =~ ^[0-9a-f]{64}$ ]] || {
        echo "Could not produce a canonical SHA-256 digest for $output" >&2
        return 1
    }
    # Update signatures cover these exact 64 lowercase ASCII characters.
    printf '%s' "$digest" > "${output}.sha256"
}

build_binary linux amd64 ""
build_binary linux arm64 ""
build_binary windows amd64 ".exe"
install -m 0644 "$ROOT_DIR/packaging/telrad-relay.service" "$OUTPUT_DIR/telrad-relay.service"
install -m 0644 "$ROOT_DIR/LICENSE" "$OUTPUT_DIR/LICENSE"
install -m 0644 "$ROOT_DIR/NOTICE" "$OUTPUT_DIR/NOTICE"
install -m 0644 "$ROOT_DIR/THIRD_PARTY_NOTICES.md" "$OUTPUT_DIR/THIRD_PARTY_NOTICES.md"
cat > "$OUTPUT_DIR/installation-manifest.json" <<EOF
{
  "schemaVersion": 1,
  "releaseVersion": "$VERSION",
  "components": {
    "configuration": 3,
    "linuxService": 2,
    "windowsService": 3,
    "windowsFirewall": 1,
    "updateTrust": 2
  }
}
EOF

printf 'Release artifacts written to %s\n' "$OUTPUT_DIR"
