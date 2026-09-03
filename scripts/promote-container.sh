#!/usr/bin/env bash
set -euo pipefail

CONTAINER_CLI="${CONTAINER_CLI:-docker}"

usage() {
    echo "usage: scripts/promote-container.sh check <image> <stable-tag>" >&2
    echo "       scripts/promote-container.sh promote-stable <image> <prerelease-tag> <stable-tag> <digest>" >&2
    echo "       scripts/promote-container.sh promote-latest <image> <stable-tag> <digest>" >&2
    exit 2
}

validate_image() {
    local image="$1"
    [[ "$image" =~ ^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$ ]] || {
        echo "Image must be an untagged fully qualified image name." >&2
        exit 1
    }
}

validate_stable_tag() {
    local tag="$1"
    [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
        echo "Stable tag must be SemVer without prerelease or build metadata." >&2
        exit 1
    }
}

refuse_existing_tag() {
    local reference="$1"
    local error_file
    error_file="$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/relay-image-inspect.XXXXXX")"
    if "$CONTAINER_CLI" buildx imagetools inspect "$reference" > /dev/null 2>"$error_file"; then
        rm -f "$error_file"
        echo "Refusing to replace immutable image tag $reference." >&2
        exit 1
    fi
    if ! grep -Eiq 'manifest unknown|not found' "$error_file"; then
        cat "$error_file" >&2
        rm -f "$error_file"
        echo "Could not prove that immutable image tag $reference is unused." >&2
        exit 1
    fi
    rm -f "$error_file"
}

resolve_digest() {
    local reference="$1"
    local output digest
    output="$("$CONTAINER_CLI" buildx imagetools inspect "$reference")"
    digest="$(awk '/^Digest:/ {print $2; exit}' <<< "$output")"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
        echo "Could not resolve a canonical digest for $reference." >&2
        exit 1
    }
    printf '%s' "$digest"
}

validate_digest() {
    local digest="$1"
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
        echo "Digest must be canonical lowercase sha256." >&2
        exit 1
    }
}

copy_digest() (
    local source_ref="$1"
    local target_ref="$2"
    local expected_digest="$3"
    local metadata_file created_digest target_digest

    metadata_file="$(mktemp "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/relay-image-promotion.XXXXXX.json")"
    cleanup() {
        rm -f "$metadata_file"
    }
    trap cleanup EXIT
    "$CONTAINER_CLI" buildx imagetools create \
        --prefer-index=false \
        --tag "$target_ref" \
        --metadata-file "$metadata_file" \
        "$source_ref"

    created_digest="$(jq --exit-status --raw-output '."containerimage.descriptor".digest' "$metadata_file")"
    target_digest="$(resolve_digest "$target_ref")"
    [[ "$created_digest" == "$expected_digest" && "$target_digest" == "$expected_digest" ]] || {
        echo "Container promotion to $target_ref did not preserve digest $expected_digest." >&2
        exit 1
    }
)

command="${1:-}"
case "$command" in
    check)
        [[ $# -eq 3 ]] || usage
        image="$2"
        stable_tag="$3"
        validate_image "$image"
        validate_stable_tag "$stable_tag"
        refuse_existing_tag "$image:${stable_tag#v}"
        ;;
    promote-stable)
        [[ $# -eq 5 ]] || usage
        image="$2"
        prerelease_tag="$3"
        stable_tag="$4"
        digest="$5"
        validate_image "$image"
        validate_stable_tag "$stable_tag"
        [[ "$prerelease_tag" == "$stable_tag"-* ]] || {
            echo "Prerelease tag $prerelease_tag does not belong to $stable_tag." >&2
            exit 1
        }
        validate_digest "$digest"

        source_ref="$image@$digest"
        prerelease_ref="$image:${prerelease_tag#v}"
        stable_ref="$image:${stable_tag#v}"
        source_digest="$(resolve_digest "$source_ref")"
        prerelease_digest="$(resolve_digest "$prerelease_ref")"
        [[ "$source_digest" == "$digest" && "$prerelease_digest" == "$digest" ]] || {
            echo "Prerelease source references no longer resolve to recorded digest $digest." >&2
            exit 1
        }
        refuse_existing_tag "$stable_ref"

        copy_digest "$source_ref" "$stable_ref" "$digest"
        ;;
    promote-latest)
        [[ $# -eq 4 ]] || usage
        image="$2"
        stable_tag="$3"
        digest="$4"
        validate_image "$image"
        validate_stable_tag "$stable_tag"
        validate_digest "$digest"

        source_ref="$image@$digest"
        stable_ref="$image:${stable_tag#v}"
        latest_ref="$image:latest"
        source_digest="$(resolve_digest "$source_ref")"
        stable_digest="$(resolve_digest "$stable_ref")"
        [[ "$source_digest" == "$digest" && "$stable_digest" == "$digest" ]] || {
            echo "Stable source references no longer resolve to recorded digest $digest." >&2
            exit 1
        }

        copy_digest "$source_ref" "$latest_ref" "$digest"
        ;;
    *)
        usage
        ;;
esac
