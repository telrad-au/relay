#!/usr/bin/env bash
# Writes <binary>.sha256 for each Relay binary and SHA256SUMS for every file in
# a release directory. Rerun it after anything changes a release file, such as
# Authenticode signing or adding SBOMs.
set -euo pipefail

release_dir="${1:?usage: scripts/write-checksums.sh <release-dir>}"
cd "$release_dir"
rm -f SHA256SUMS ./*.sha256
for binary in telrad-relay-linux-amd64 telrad-relay-linux-arm64 telrad-relay-windows-amd64.exe; do
    [[ -f "$binary" ]] || {
        echo "Release binary is missing: $release_dir/$binary" >&2
        exit 1
    }
    sha256sum "$binary" > "$binary.sha256"
done
find . -maxdepth 1 -type f ! -name 'SHA256SUMS*' ! -name '*.sha256' -printf '%P\n' \
    | LC_ALL=C sort \
    | xargs sha256sum > SHA256SUMS.new
mv SHA256SUMS.new SHA256SUMS
