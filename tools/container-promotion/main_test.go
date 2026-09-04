package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	testImage    = "ghcr.io/telrad-au/relay"
	testRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigest   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestCreateMetadata(t *testing.T) {
	var output bytes.Buffer
	err := runCreate([]string{
		"--prerelease", "v0.1.0-alpha.1",
		"--revision", testRevision,
		"--image", testImage,
		"--digest", testDigest,
		"--platform", "linux/amd64",
		"--platform", "linux/arm64",
		"--workflow-run-id", "123",
		"--workflow-run-attempt", "1",
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	var metadata promotionMetadata
	if err := json.Unmarshal(output.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.StableVersion != "v0.1.0" || metadata.Digest != testDigest {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
}

func TestSelectCandidatesUsesHighestSemVerPrerelease(t *testing.T) {
	directory := t.TempDir()
	paths := []string{
		writeMetadata(t, directory, validMetadata("v0.1.0-alpha.10")),
		writeMetadata(t, directory, validMetadata("v0.1.0-rc.1")),
		writeMetadata(t, directory, validMetadata("v0.1.0-alpha.2")),
		writeMetadata(t, directory, validMetadata("v0.1.0-beta.3")),
	}
	candidates, err := selectCandidates("v0.1.0", testRevision, testImage, paths, ioDiscard{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v0.1.0-rc.1", "v0.1.0-beta.3", "v0.1.0-alpha.10", "v0.1.0-alpha.2"}
	for index, candidate := range candidates {
		if candidate.Prerelease != want[index] {
			t.Fatalf("candidate %d = %s, want %s", index, candidate.Prerelease, want[index])
		}
	}
}

func TestSelectCandidatesRejectsIneligibleMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*promotionMetadata)
		stable string
		paths  bool
		want   string
	}{
		{
			name: "mismatched source SHA",
			mutate: func(metadata *promotionMetadata) {
				metadata.Revision = "cccccccccccccccccccccccccccccccccccccccc"
			},
			stable: "v0.1.0",
			paths:  true,
			want:   "no valid prerelease promotion metadata",
		},
		{
			name: "mismatched base version",
			mutate: func(metadata *promotionMetadata) {
				metadata.Prerelease = "v0.2.0-alpha.1"
				metadata.StableVersion = "v0.2.0"
			},
			stable: "v0.1.0",
			paths:  true,
			want:   "no valid prerelease promotion metadata",
		},
		{
			name:   "missing metadata",
			mutate: func(*promotionMetadata) {},
			stable: "v0.1.0",
			paths:  false,
			want:   "no prerelease contains container-promotion.json",
		},
		{
			name: "malformed digest",
			mutate: func(metadata *promotionMetadata) {
				metadata.Digest = "sha256:not-a-digest"
			},
			stable: "v0.1.0",
			paths:  true,
			want:   "no valid prerelease promotion metadata",
		},
		{
			name: "missing architecture",
			mutate: func(metadata *promotionMetadata) {
				metadata.Platforms = []string{"linux/amd64"}
			},
			stable: "v0.1.0",
			paths:  true,
			want:   "no valid prerelease promotion metadata",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := validMetadata("v0.1.0-alpha.1")
			test.mutate(&metadata)
			var paths []string
			if test.paths {
				paths = []string{writeMetadata(t, t.TempDir(), metadata)}
			}
			_, err := selectCandidates(test.stable, testRevision, testImage, paths, ioDiscard{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestStableWorkflowDoesNotBuildContainer(t *testing.T) {
	workflowPath, err := filepath.Abs("../../.github/workflows/publish-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(content)
	for _, forbidden := range []string{"docker/build-push-action", "docker build ", "buildx build"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("stable workflow contains forbidden container build operation %q", forbidden)
		}
	}
	if !strings.Contains(workflow, `scripts/promote-container.sh promote-stable`) {
		t.Fatal("stable workflow does not invoke digest promotion")
	}
}

func TestStableWorkflowPlansBeforeApprovalAndFinalizesLatestLast(t *testing.T) {
	workflowPath, err := filepath.Abs("../../.github/workflows/publish-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(content)
	for _, required := range []string{
		"group: publish-stable",
		"permissions: {}",
		"needs: plan",
		"persist-credentials: false",
		"scripts/select-container-promotion.sh",
		"go run ./tools/container-promotion guard-stable",
		"azure/login@7ddb5af1ef8758cf1353cf3b42f940aee27ba21c",
		"azure/artifact-signing-action@c7ab2a863ab5f9a846ddb8265964877ef296ee82",
		"actions/upload-artifact@bbbca2ddaa5d8feaa63e36b76fdaad77386f024f",
		"actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("stable workflow does not contain %q", required)
		}
	}
	plan := strings.Index(workflow, "    plan:")
	approval := strings.Index(workflow, "        environment: production-release")
	stable := strings.Index(workflow, "scripts/promote-container.sh promote-stable")
	release := strings.Index(workflow, "gh release edit \"$TAG\" --draft=false --latest")
	latest := strings.Index(workflow, "scripts/promote-container.sh promote-latest")
	if plan < 0 || approval < 0 || stable < 0 || release < 0 || latest < 0 || !(plan < approval && approval < stable && stable < release && release < latest) {
		t.Fatalf("unsafe stable workflow ordering: plan=%d approval=%d stable=%d release=%d latest=%d", plan, approval, stable, release, latest)
	}
	buildStart := strings.Index(workflow, "    build-native:")
	signingStart := strings.Index(workflow, "    sign-native:")
	attestationStart := strings.Index(workflow, "    attest-native:")
	publishStart := strings.Index(workflow, "    publish:")
	if buildStart < 0 || signingStart < 0 || attestationStart < 0 || publishStart < 0 ||
		!(plan < buildStart && buildStart < signingStart && signingStart < attestationStart && attestationStart < publishStart) {
		t.Fatalf("stable workflow jobs are not ordered plan, build-native, sign-native, attest-native, publish")
	}
	signingJob := workflow[signingStart:publishStart]
	if strings.Contains(signingJob, "contents: write") || strings.Contains(signingJob, "packages: write") {
		t.Fatal("production signing job can publish repository or package content")
	}
	publishingJob := workflow[publishStart:]
	if strings.Contains(publishingJob, "secrets.RELAY_") {
		t.Fatal("publication job has access to Relay signing secrets")
	}
}

func TestStableWorkflowUsesOIDCBackedArtifactSigning(t *testing.T) {
	content, err := os.ReadFile("../../.github/workflows/publish-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(content)
	for _, required := range []string{
		"runs-on: windows-2025",
		"environment: production-release",
		"id-token: write",
		"secrets.AZURE_ARTIFACT_SIGNING_CLIENT_ID",
		"secrets.AZURE_ARTIFACT_SIGNING_TENANT_ID",
		"secrets.AZURE_ARTIFACT_SIGNING_SUBSCRIPTION_ID",
		"secrets.AZURE_ARTIFACT_SIGNING_ENDPOINT",
		"secrets.AZURE_ARTIFACT_SIGNING_ACCOUNT_NAME",
		"secrets.AZURE_ARTIFACT_SIGNING_CERTIFICATE_PROFILE_NAME",
		"timestamp-rfc3161: http://timestamp.acs.microsoft.com",
		"scripts/verify-windows-signature.ps1",
		"scripts/finalize-signed-release.sh",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("stable workflow does not contain Azure Artifact Signing contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"RELAY_WINDOWS_SIGNING_PFX_BASE64",
		"RELAY_WINDOWS_SIGNING_PASSWORD",
		"RELAY_WINDOWS_TIMESTAMP_URL",
		"azure-client-secret",
		"osslsigncode",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("stable workflow still contains exportable signing material or legacy signer %q", forbidden)
		}
	}
}

func TestAzureSigningSmokeWorkflowCannotPublish(t *testing.T) {
	content, err := os.ReadFile("../../.github/workflows/test-azure-signing.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(content)
	for _, required := range []string{
		"workflow_dispatch:",
		"permissions: {}",
		"runs-on: windows-2025",
		"environment: artifact-signing-test",
		"contents: read",
		"id-token: write",
		"persist-credentials: false",
		"azure/login@7ddb5af1ef8758cf1353cf3b42f940aee27ba21c",
		"azure/artifact-signing-action@c7ab2a863ab5f9a846ddb8265964877ef296ee82",
		"timestamp-rfc3161: http://timestamp.acs.microsoft.com",
		"scripts/verify-windows-signature.ps1",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Azure signing smoke workflow does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"\n    push:",
		"\n    pull_request:",
		"\n    schedule:",
		"contents: write",
		"packages: write",
		"actions/upload-artifact",
		"gh release",
		"promote-container.sh",
		"secrets.RELAY_",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("Azure signing smoke workflow contains publication capability %q", forbidden)
		}
	}
}

func TestWorkflowsDoNotPersistCheckoutCredentials(t *testing.T) {
	for _, relativePath := range []string{
		"../../.github/workflows/ci.yml",
		"../../.github/workflows/codeql.yml",
		"../../.github/workflows/publish-prerelease.yml",
		"../../.github/workflows/publish-release.yml",
		"../../.github/workflows/publish-testing.yml",
		"../../.github/workflows/test-azure-signing.yml",
	} {
		content, err := os.ReadFile(relativePath)
		if err != nil {
			t.Fatal(err)
		}
		workflow := string(content)
		checkouts := strings.Count(workflow, "actions/checkout@")
		nonPersisted := strings.Count(workflow, "persist-credentials: false")
		if checkouts == 0 || checkouts != nonPersisted {
			t.Errorf("%s has %d checkouts but %d non-persisted credential settings", relativePath, checkouts, nonPersisted)
		}
	}
}

func TestReleaseSigningJobsCannotPublish(t *testing.T) {
	tests := []struct {
		path          string
		signingJob    string
		publishingJob string
		secret        string
	}{
		{"../../.github/workflows/publish-release.yml", "    sign-native:", "    publish:", "secrets.RELAY_UPDATE_SIGNING_KEY_BASE64"},
		{"../../.github/workflows/publish-prerelease.yml", "    build_native:", "    publish:", "secrets.RELAY_DEV_UPDATE_SIGNING_KEY_BASE64"},
		{"../../.github/workflows/publish-testing.yml", "    build:", "    publish:", "secrets.RELAY_TESTING_UPDATE_SIGNING_KEY_BASE64"},
	}
	for _, test := range tests {
		content, err := os.ReadFile(test.path)
		if err != nil {
			t.Fatal(err)
		}
		workflow := string(content)
		signingStart := strings.Index(workflow, test.signingJob)
		publishingStart := strings.Index(workflow, test.publishingJob)
		if signingStart < 0 || publishingStart < 0 || signingStart >= publishingStart {
			t.Fatalf("%s does not separate signing and publication jobs", test.path)
		}
		signing := workflow[signingStart:publishingStart]
		publishing := workflow[publishingStart:]
		if !strings.Contains(signing, test.secret) {
			t.Fatalf("%s signing job does not contain expected isolated key", test.path)
		}
		if strings.Contains(signing, "contents: write") || strings.Contains(signing, "packages: write") {
			t.Fatalf("%s signing job has publication permissions", test.path)
		}
		if strings.Contains(publishing, "secrets.RELAY_") {
			t.Fatalf("%s publication job receives a Relay private key", test.path)
		}
	}
}

func TestMutableReleaseChannelsAreSerialized(t *testing.T) {
	for path, group := range map[string]string{
		"../../.github/workflows/publish-prerelease.yml": "group: publish-prerelease",
		"../../.github/workflows/publish-release.yml":    "group: publish-stable",
		"../../.github/workflows/publish-testing.yml":    "group: publish-testing-main",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), group) || !strings.Contains(string(content), "cancel-in-progress: false") {
			t.Errorf("%s does not serialize its mutable release channel", path)
		}
	}
}

func TestPromoteScriptRejectsExistingStableTag(t *testing.T) {
	requireBash(t)
	command, marker := fakeContainerCLI(t)
	process := exec.Command("bash", promoteScriptPath(t), "promote-stable", testImage, "v0.1.0-alpha.1", "v0.1.0", testDigest)
	process.Env = append(os.Environ(),
		"CONTAINER_CLI="+command,
		"FAKE_CREATE_MARKER="+marker,
		"FAKE_EXISTING_STABLE=1",
		"FAKE_EXPECTED_DIGEST="+testDigest,
	)
	output, err := process.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Refusing to replace immutable image tag") {
		t.Fatalf("error = %v, output = %s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("promotion unexpectedly created marker: %v", err)
	}
}

func TestPromoteScriptPreservesDigest(t *testing.T) {
	requireBash(t)
	command, marker := fakeContainerCLI(t)
	process := exec.Command("bash", promoteScriptPath(t), "promote-stable", testImage, "v0.1.0-alpha.1", "v0.1.0", testDigest)
	process.Env = append(os.Environ(),
		"CONTAINER_CLI="+command,
		"FAKE_CREATE_MARKER="+marker,
		"FAKE_EXPECTED_DIGEST="+testDigest,
	)
	if output, err := process.CombinedOutput(); err != nil {
		t.Fatalf("error = %v, output = %s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("promotion did not call imagetools create: %v", err)
	}
}

func TestPromoteScriptRejectsChangedDigest(t *testing.T) {
	requireBash(t)
	command, marker := fakeContainerCLI(t)
	process := exec.Command("bash", promoteScriptPath(t), "promote-stable", testImage, "v0.1.0-alpha.1", "v0.1.0", testDigest)
	process.Env = append(os.Environ(),
		"CONTAINER_CLI="+command,
		"FAKE_CREATE_MARKER="+marker,
		"FAKE_EXPECTED_DIGEST="+testDigest,
		"FAKE_PROMOTED_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	)
	output, err := process.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "did not preserve") {
		t.Fatalf("error = %v, output = %s", err, output)
	}
}

func TestPromoteLatestUsesVerifiedStableDigest(t *testing.T) {
	requireBash(t)
	command, marker := fakeContainerCLI(t)
	process := exec.Command("bash", promoteScriptPath(t), "promote-latest", testImage, "v0.1.0", testDigest)
	process.Env = append(os.Environ(),
		"CONTAINER_CLI="+command,
		"FAKE_CREATE_MARKER="+marker,
		"FAKE_EXISTING_STABLE=1",
		"FAKE_EXPECTED_DIGEST="+testDigest,
		"FAKE_EXPECTED_TARGET="+testImage+":latest",
	)
	if output, err := process.CombinedOutput(); err != nil {
		t.Fatalf("error = %v, output = %s", err, output)
	}
	created, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(created)) != testImage+":latest" {
		t.Fatalf("created target = %q", created)
	}
}

func TestPromoteLatestRejectsChangedStableDigest(t *testing.T) {
	requireBash(t)
	command, marker := fakeContainerCLI(t)
	process := exec.Command("bash", promoteScriptPath(t), "promote-latest", testImage, "v0.1.0", testDigest)
	process.Env = append(os.Environ(),
		"CONTAINER_CLI="+command,
		"FAKE_CREATE_MARKER="+marker,
		"FAKE_EXISTING_STABLE=1",
		"FAKE_EXPECTED_DIGEST="+testDigest,
		"FAKE_PROMOTED_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	)
	output, err := process.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "no longer resolve") {
		t.Fatalf("error = %v, output = %s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("latest promotion unexpectedly created marker: %v", err)
	}
}

func validMetadata(prerelease string) promotionMetadata {
	version, err := parsePrerelease(prerelease)
	if err != nil {
		panic(err)
	}
	return promotionMetadata{
		SchemaVersion:      schemaVersion,
		Prerelease:         prerelease,
		StableVersion:      version.stable,
		Revision:           testRevision,
		Image:              testImage,
		Digest:             testDigest,
		Platforms:          []string{"linux/amd64", "linux/arm64"},
		WorkflowRunID:      123,
		WorkflowRunAttempt: 1,
	}
}

func writeMetadata(t *testing.T, directory string, metadata promotionMetadata) string {
	t.Helper()
	path := filepath.Join(directory, metadata.Prerelease+".json")
	content, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func promoteScriptPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../scripts/promote-container.sh")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("promotion script is exercised by the Linux CI job")
	}
}

func fakeContainerCLI(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	command := filepath.Join(directory, "container-cli")
	marker := filepath.Join(directory, "created")
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "buildx" && "$2" == "imagetools" ]] || exit 90
operation="$3"
shift 3
if [[ "$operation" == "inspect" ]]; then
    reference="$1"
    if [[ "$reference" == %q ]]; then
        if [[ "${FAKE_EXISTING_STABLE:-}" == "1" ]] || { [[ -f "$FAKE_CREATE_MARKER" ]] && grep -Fqx "$reference" "$FAKE_CREATE_MARKER"; }; then
            printf 'Name: %%s\nDigest: %%s\n' "$reference" "${FAKE_PROMOTED_DIGEST:-$FAKE_EXPECTED_DIGEST}"
            exit 0
        fi
        printf 'manifest unknown\n' >&2
        exit 1
    fi
    if [[ "$reference" == %q ]]; then
        if [[ -f "$FAKE_CREATE_MARKER" ]] && grep -Fqx "$reference" "$FAKE_CREATE_MARKER"; then
            printf 'Name: %%s\nDigest: %%s\n' "$reference" "${FAKE_PROMOTED_DIGEST:-$FAKE_EXPECTED_DIGEST}"
            exit 0
        fi
        printf 'manifest unknown\n' >&2
        exit 1
    fi
    printf 'Name: %%s\nDigest: %%s\n' "$reference" "$FAKE_EXPECTED_DIGEST"
    exit 0
fi
if [[ "$operation" == "create" ]]; then
    metadata_file=""
    saw_prefer_index="false"
    target=""
    saw_source="false"
    while (( $# > 0 )); do
        case "$1" in
            --prefer-index=false)
                saw_prefer_index="true"
                ;;
            --tag)
                shift
                [[ -z "$target" ]] || exit 93
                target="$1"
                ;;
            --metadata-file)
                shift
                metadata_file="$1"
                ;;
            *@sha256:*)
                [[ "$1" == %q"$FAKE_EXPECTED_DIGEST" ]] && saw_source="true"
                ;;
        esac
        shift
    done
    expected_target="${FAKE_EXPECTED_TARGET:-%s}"
    [[ "$saw_prefer_index" == "true" && "$target" == "$expected_target" && "$saw_source" == "true" && -n "$metadata_file" ]] || exit 92
    printf '%%s\n' "$target" > "$FAKE_CREATE_MARKER"
    printf '{"containerimage.descriptor":{"digest":"%%s"}}\n' "${FAKE_PROMOTED_DIGEST:-$FAKE_EXPECTED_DIGEST}" > "$metadata_file"
    exit 0
fi
exit 91
`, testImage+":0.1.0", testImage+":latest", testImage+"@", testImage+":0.1.0")
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return command, marker
}

type ioDiscard struct{}

func (ioDiscard) Write(content []byte) (int, error) {
	return len(content), nil
}
