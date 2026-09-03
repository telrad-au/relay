package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromotionCommandWrappers(t *testing.T) {
	var output bytes.Buffer
	if err := runValidate([]string{"--prerelease", "v0.1.0-rc.1"}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "v0.1.0\n" {
		t.Fatalf("validate output = %q", output.String())
	}
	if err := runValidate([]string{"--prerelease", "v0.1.0-rc.1", "unexpected"}, &output); err == nil {
		t.Fatal("validate accepted a positional argument")
	}
	if err := runCreate([]string{"unexpected"}, &output); err == nil {
		t.Fatal("create accepted a positional argument")
	}

	metadataPath := writeMetadata(t, t.TempDir(), validMetadata("v0.1.0-rc.1"))
	output.Reset()
	if err := runSelect([]string{
		"--stable", "v0.1.0",
		"--revision", testRevision,
		"--image", testImage,
		metadataPath,
	}, &output, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var selected []promotionMetadata
	if err := json.Unmarshal(output.Bytes(), &selected); err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Prerelease != "v0.1.0-rc.1" {
		t.Fatalf("selected metadata = %+v", selected)
	}
}

func TestGuardStableRequiresMonotonicVersions(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		existing  []string
		wantError string
	}{
		{name: "first release", candidate: "v0.1.0"},
		{name: "new patch", candidate: "v0.1.1", existing: []string{"v0.1.0"}},
		{name: "new major", candidate: "v10.0.0", existing: []string{"v2.99.99", "v10.0.0"}},
		{name: "older", candidate: "v0.1.0", existing: []string{"v0.2.0"}, wantError: "must be newer"},
		{name: "same version under another tag entry", candidate: "v1.2.3", existing: []string{"v1.2.3"}},
		{name: "invalid candidate", candidate: "v1.2.3-rc.1", wantError: "stable tag"},
		{name: "invalid existing tag", candidate: "v1.2.3", existing: []string{"v1.0"}, wantError: "existing stable tag"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"--candidate", test.candidate}
			args = append(args, test.existing...)
			var output bytes.Buffer
			err := runGuardStable(args, &output)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				if output.String() != test.candidate+"\n" {
					t.Fatalf("output = %q", output.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantError)
			}
		})
	}
}

func TestPromotionMetadataRejectsUnsafeFields(t *testing.T) {
	tests := []struct {
		name     string
		stable   string
		revision string
		image    string
		mutate   func(*promotionMetadata)
		want     string
	}{
		{"schema", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.SchemaVersion = 2 }, "schemaVersion"},
		{"prerelease", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.Prerelease = "v0.1.0-alpha+build" }, "invalid identifier"},
		{"stable derived from prerelease", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.StableVersion = "v0.2.0" }, "does not match prerelease base"},
		{"requested stable", "v0.2.0", testRevision, testImage, func(*promotionMetadata) {}, "does not match requested release"},
		{"revision syntax", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.Revision = "ABC" }, "lowercase Git commit SHA"},
		{"revision identity", "v0.1.0", strings.Repeat("c", 40), testImage, func(*promotionMetadata) {}, "does not match stable source"},
		{"image syntax", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.Image = "GHCR.IO/TELRAD/RELAY" }, "untagged fully qualified"},
		{"image identity", "v0.1.0", testRevision, "ghcr.io/telrad-au/other", func(*promotionMetadata) {}, "does not match expected image"},
		{"digest", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.Digest = "sha256:ABC" }, "canonical lowercase sha256"},
		{"invalid platform", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.Platforms = append(value.Platforms, "linux/AMD64") }, "invalid platform"},
		{"duplicate platform", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.Platforms = append(value.Platforms, "linux/amd64") }, "duplicate platform"},
		{"missing platform", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.Platforms = []string{"linux/amd64"} }, "required platform"},
		{"workflow run", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.WorkflowRunID = 0 }, "workflowRunId"},
		{"workflow attempt", "v0.1.0", testRevision, testImage, func(value *promotionMetadata) { value.WorkflowRunAttempt = 0 }, "workflowRunAttempt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := validMetadata("v0.1.0-alpha.1")
			test.mutate(&metadata)
			err := validateMetadata(metadata, test.stable, test.revision, test.image)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestPromotionSelectionRejectsUnsafeInputs(t *testing.T) {
	path := writeMetadata(t, t.TempDir(), validMetadata("v0.1.0-alpha.1"))
	tests := []struct {
		name     string
		stable   string
		revision string
		image    string
		want     string
	}{
		{"stable", "v0.1.0-rc.1", testRevision, testImage, "stable tag"},
		{"revision", "v0.1.0", "ABC", testImage, "stable revision"},
		{"image", "v0.1.0", testRevision, testImage + ":latest", "untagged fully qualified"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := selectCandidates(test.stable, test.revision, test.image, []string{path}, ioDiscard{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestReadPromotionMetadataRejectsUnknownAndTrailingJSON(t *testing.T) {
	valid, err := json.Marshal(validMetadata("v0.1.0-alpha.1"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
	}{
		{"unknown field", append(valid[:len(valid)-1], []byte(`,"unknown":true}`)...)},
		{"trailing value", append(append([]byte{}, valid...), []byte(` {}`)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.json")
			if err := os.WriteFile(path, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readMetadata(path); err == nil {
				t.Fatal("malformed metadata was accepted")
			}
		})
	}
}

func TestSelectionRejectsAssetIdentityMismatchAndIgnoresDuplicate(t *testing.T) {
	metadata := validMetadata("v0.1.0-alpha.1")
	directory := t.TempDir()
	mismatchPath := filepath.Join(directory, "v0.1.0-alpha.2.json")
	content, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mismatchPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	if _, err := selectCandidates("v0.1.0", testRevision, testImage, []string{mismatchPath}, &diagnostics); err == nil {
		t.Fatal("asset identity mismatch was accepted")
	}
	if !strings.Contains(diagnostics.String(), "asset belongs to") {
		t.Fatalf("diagnostics = %q", diagnostics.String())
	}

	first := writeMetadata(t, t.TempDir(), metadata)
	second := writeMetadata(t, t.TempDir(), metadata)
	diagnostics.Reset()
	candidates, err := selectCandidates("v0.1.0", testRevision, testImage, []string{first, second}, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !strings.Contains(diagnostics.String(), "duplicate metadata") {
		t.Fatalf("candidates = %+v, diagnostics = %q", candidates, diagnostics.String())
	}
}

func TestPrereleaseSemverValidationAndOrdering(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v0.1.0-", "v0.1.0-alpha..1", "v0.1.0-alpha+build", "v0.1.0-alpha.01"} {
		if _, err := parsePrerelease(tag); err == nil {
			t.Errorf("invalid prerelease %q was accepted", tag)
		}
	}
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{"v0.1.0-alpha.2", "v0.1.0-alpha.10", -1},
		{"v0.1.0-alpha", "v0.1.0-alpha.1", -1},
		{"v0.1.0-1", "v0.1.0-alpha", -1},
		{"v0.1.0-rc.1", "v0.1.0-beta.9", 1},
		{"v0.1.0-alpha", "v0.1.0-alpha", 0},
	}
	for _, test := range tests {
		left, err := parsePrerelease(test.left)
		if err != nil {
			t.Fatal(err)
		}
		right, err := parsePrerelease(test.right)
		if err != nil {
			t.Fatal(err)
		}
		if got := comparePrerelease(left, right); got != test.want {
			t.Errorf("comparePrerelease(%s, %s) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}
