#!/usr/bin/env bash
set -euo pipefail

readonly maven_image='maven:3.9.11-eclipse-temurin-21@sha256:6fdc855a6ed81d288ca7ca37ac6ff5e9308b612485c0801d70b25a858c83d237'
readonly source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

docker run --rm \
    --volume "${source_root}:/source:ro" \
    --workdir /source/tools/hl7-validation \
    "${maven_image}" \
    mvn --batch-mode --no-transfer-progress process-classes
