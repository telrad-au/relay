package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTestingLinuxInstallerHasStateAwareUpdateExperience(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	packagingDir := filepath.Join(filepath.Dir(sourceFile), "..", "..", "packaging")
	installerData, err := os.ReadFile(filepath.Join(packagingDir, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{"install-native", "update-trust.json", "installation-manifest.json"} {
		if !strings.Contains(installer, required) {
			t.Errorf("installer missing %s", required)
		}
	}

	bootstrapData, err := os.ReadFile(filepath.Join(packagingDir, "install-testing.sh.template"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bootstrapData), "Testing build installed. Run: telrad") {
		t.Fatal("testing Linux bootstrap duplicates the bundle install result")
	}
	if !strings.Contains(string(bootstrapData), "expected_archive_sha256") || !strings.Contains(string(bootstrapData), "sha256sum") {
		t.Fatal("testing Linux bootstrap does not pin its exact platform archive")
	}
	windowsBootstrapData, err := os.ReadFile(filepath.Join(packagingDir, "install-testing.ps1.template"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(windowsBootstrapData), "Testing build installed. Run: telrad") {
		t.Fatal("testing Windows bootstrap duplicates the bundle install result")
	}
	if !strings.Contains(string(windowsBootstrapData), "$expectedArchiveSha256") || !strings.Contains(string(windowsBootstrapData), "Get-FileHash") {
		t.Fatal("testing Windows bootstrap does not pin its exact platform archive")
	}

	workflowData, err := os.ReadFile(filepath.Join(packagingDir, "..", ".github", "workflows", "publish-testing.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowData)
	for _, required := range []string{
		"    workflow_dispatch:",
		"environment: testing-release",
		"GH_REPO: ${{ github.repository }}",
		"GITHUB_RUN_ID",
		"RELAY_TESTING_UPDATE_SIGNING_KEY_BASE64",
		"vars.RELAY_TESTING_UPDATE_PUBLIC_KEY_BASE64",
		`openssl pkey -pubin -inform DER -in "$trusted_public_key_der" -out "$trusted_public_key"`,
		"scripts/build-testing-release.sh",
		`install -m 0644 "$asset_dir/update-trust.json" "$bundle/update-trust.json"`,
		`install -m 0644 "$asset_dir/update-trust.json" "$windows_bundle/update-trust.json"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("testing publication workflow does not contain %q", required)
		}
	}
	if strings.Contains(workflow, "secrets.RELAY_TESTING_UPDATE_PUBLIC_KEY_BASE64") {
		t.Error("testing publication treats the distributable public key as a secret instead of an environment variable")
	}
	if strings.Contains(workflow, "\n    push:") {
		t.Error("testing publication workflow must not run automatically on pushes")
	}
}
