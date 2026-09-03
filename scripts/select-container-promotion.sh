#!/usr/bin/env bash
set -euo pipefail

usage() {
    echo "usage: scripts/select-container-promotion.sh <stable-tag> <image>" >&2
    exit 2
}

[[ $# -eq 2 ]] || usage
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"

stable_tag="$1"
image="$2"
revision="$(git rev-parse "$stable_tag^{commit}")"
promotion_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/relay-container-promotions.XXXXXX")"
cleanup() {
    rm -rf "$promotion_dir"
}
trap cleanup EXIT

releases="$promotion_dir/releases.tsv"
gh api --paginate "repos/$GITHUB_REPOSITORY/releases?per_page=100" \
    --jq '.[] | select(.prerelease == true and .draft == false) | [.tag_name, ([.assets[] | select(.name == "container-promotion.json") | .url][0] // "")] | @tsv' \
    > "$releases"

metadata_files=()
while IFS=$'\t' read -r prerelease asset_url; do
    [[ "$prerelease" == "$stable_tag"-* ]] || continue
    [[ "$prerelease" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z][0-9A-Za-z.-]*$ ]] || {
        echo "Ignoring unsafe prerelease tag from GitHub Releases: $prerelease" >&2
        continue
    }
    if [[ -z "$asset_url" ]]; then
        echo "Ignoring $prerelease: container-promotion.json is missing." >&2
        continue
    fi
    destination="$promotion_dir/$prerelease.json"
    if ! gh api \
        --header 'Accept: application/octet-stream' \
        "$asset_url" > "$destination"; then
        echo "Ignoring $prerelease: promotion metadata could not be downloaded." >&2
        rm -f "$destination"
        continue
    fi
    metadata_files+=("$destination")
done < "$releases"

candidates="$promotion_dir/candidates.json"
go run ./tools/container-promotion select \
    --stable "$stable_tag" \
    --revision "$revision" \
    --image "$image" \
    "${metadata_files[@]}" \
    > "$candidates"

while IFS= read -r candidate; do
    prerelease="$(jq --raw-output .prerelease <<< "$candidate")"
    digest="$(jq --raw-output .digest <<< "$candidate")"
    run_id="$(jq --raw-output .workflowRunId <<< "$candidate")"
    run_attempt="$(jq --raw-output .workflowRunAttempt <<< "$candidate")"

    tag_revision="$(git rev-parse "$prerelease^{commit}" 2>/dev/null || true)"
    if [[ "$tag_revision" != "$revision" ]]; then
        echo "Ignoring $prerelease: Git tag does not resolve to $revision." >&2
        continue
    fi

    run="$promotion_dir/$prerelease-run.json"
    if ! gh api \
        "repos/$GITHUB_REPOSITORY/actions/runs/$run_id/attempts/$run_attempt" \
        > "$run"; then
        echo "Ignoring $prerelease: publication workflow run is unavailable." >&2
        continue
    fi
    if ! jq --exit-status \
        --arg prerelease "$prerelease" \
        --arg revision "$revision" \
        --arg workflow '.github/workflows/publish-prerelease.yml' \
        --argjson attempt "$run_attempt" \
        '(.status == "completed") and
         (.conclusion == "success") and
         (.event == "push") and
         (.head_branch == $prerelease) and
         (.head_sha == $revision) and
         (.run_attempt == $attempt) and
         ((.path == $workflow) or (.path | startswith($workflow + "@")))' \
        "$run" >/dev/null; then
        echo "Ignoring $prerelease: publication workflow did not complete successfully for this tag and commit." >&2
        continue
    fi

    source_inspect="$(docker buildx imagetools inspect "$image@$digest" 2>/dev/null || true)"
    prerelease_inspect="$(docker buildx imagetools inspect "$image:${prerelease#v}" 2>/dev/null || true)"
    source_digest="$(awk '/^Digest:/ {print $2; exit}' <<< "$source_inspect")"
    prerelease_digest="$(awk '/^Digest:/ {print $2; exit}' <<< "$prerelease_inspect")"
    if [[ "$source_digest" != "$digest" || "$prerelease_digest" != "$digest" ]]; then
        echo "Ignoring $prerelease: recorded digest is missing or its immutable tag has moved." >&2
        continue
    fi

    manifest="$promotion_dir/$prerelease-manifest.json"
    if ! docker buildx imagetools inspect "$image@$digest" \
        --format '{{json .Manifest}}' > "$manifest"; then
        echo "Ignoring $prerelease: manifest could not be inspected." >&2
        continue
    fi
    if ! jq --exit-status \
        'any(.manifests[]; .platform.os == "linux" and .platform.architecture == "amd64") and
         any(.manifests[]; .platform.os == "linux" and .platform.architecture == "arm64")' \
        "$manifest" >/dev/null; then
        echo "Ignoring $prerelease: registry manifest does not contain the required platforms." >&2
        continue
    fi

    jq --compact-output . <<< "$candidate"
    exit 0
done < <(jq --compact-output '.[]' "$candidates")

echo "No eligible successfully published prerelease container can be promoted to $stable_tag." >&2
exit 1
