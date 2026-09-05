#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

EXPECTED_APACHE_SHA256="cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"
EXPECTED_NOTICE_SHA256="add8897444ef7e7d214db9ac272a6aa29abdce1d8ea739c77eaec25436643ef2"

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print tolower($1)}'
    else
        shasum -a 256 "$1" | awk '{print tolower($1)}'
    fi
}

[[ "$(sha256_file LICENSE)" == "$EXPECTED_APACHE_SHA256" ]] || {
    echo "LICENSE is not the canonical Apache License 2.0 text." >&2
    exit 1
}
[[ "$(sha256_file NOTICE)" == "$EXPECTED_NOTICE_SHA256" ]] || {
    echo "NOTICE does not contain the reviewed project attribution." >&2
    exit 1
}

expected_modules="$(printf '%s\n' \
    'github.com/coder/websocket v1.8.14' \
    'go.etcd.io/bbolt v1.5.0' \
    'golang.org/x/sys v0.47.0')"
license_go_cache="${RELAY_LICENSE_GOCACHE:-${TMPDIR:-/tmp}/telrad-relay-license-go-cache}"
mkdir -p "$license_go_cache"
module_lines=""
for target in linux/amd64 linux/arm64 windows/amd64; do
    target_os="${target%/*}"
    target_arch="${target#*/}"
    target_modules="$(
        GOCACHE="$license_go_cache" GOOS="$target_os" GOARCH="$target_arch" CGO_ENABLED=0 \
            go list -mod=readonly -deps \
                -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}' \
                ./cmd/telrad-relay
    )" || {
        echo "Could not resolve the distributed Go module graph for $target." >&2
        exit 1
    }
    module_lines="${module_lines}${target_modules}"$'\n'
done
actual_modules="$(printf '%s' "$module_lines" | awk 'NF == 2' | LC_ALL=C sort -u)"

[[ "$actual_modules" == "$expected_modules" ]] || {
    echo "Distributed Go module graph differs from the audited notice set:" >&2
    diff -u \
        <(printf '%s\n' "$expected_modules") \
        <(printf '%s\n' "$actual_modules") >&2 || true
    exit 1
}

for heading in \
    '## Alpine ca-certificates-bundle 20260611-r0' \
    '## github.com/coder/websocket v1.8.14' \
    '## go.etcd.io/bbolt v1.5.0' \
    '## golang.org/x/sys v0.47.0' \
    '## Go 1.27.0 runtime and standard library'; do
    grep -Fqx "$heading" THIRD_PARTY_NOTICES.md || {
        echo "THIRD_PARTY_NOTICES.md is missing: $heading" >&2
        exit 1
    }
done

module_cache="$(go env GOMODCACHE)"
go_root="$(go env GOROOT)"
while read -r expected path; do
    [[ "$(sha256_file "$path")" == "$expected" ]] || {
        echo "Upstream licence text changed and must be reviewed: $path" >&2
        exit 1
    }
done <<EOF
cc0975a5f6305145bdd7b41ce9479632fdac3870e6ac4281f28017f18c767c4e $module_cache/github.com/coder/websocket@v1.8.14/LICENSE.txt
c15d721c37e277a11584547de6d618541501f7aa10c4e32a945a4f9ff36cb0f6 $module_cache/go.etcd.io/bbolt@v1.5.0/LICENSE
911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad $module_cache/golang.org/x/sys@v0.47.0/LICENSE
96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc $module_cache/golang.org/x/sys@v0.47.0/PATENTS
911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad $go_root/LICENSE
96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc $go_root/PATENTS
EOF

grep -Fq 'org.opencontainers.image.licenses="Apache-2.0"' Dockerfile
grep -Fq 'COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/licenses/telrad-relay/' Dockerfile
grep -Fq 'install -m 0644 "$ROOT_DIR/LICENSE" "$OUTPUT_DIR/LICENSE"' scripts/build-release.sh
grep -Fq 'install -m 0644 "$ROOT_DIR/NOTICE" "$OUTPUT_DIR/NOTICE"' scripts/build-release.sh
grep -Fq 'install -m 0644 "$ROOT_DIR/THIRD_PARTY_NOTICES.md" "$OUTPUT_DIR/THIRD_PARTY_NOTICES.md"' scripts/build-release.sh

echo "Licence and distributed dependency audit passed."
