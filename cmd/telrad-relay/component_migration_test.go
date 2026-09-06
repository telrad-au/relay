package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHostedInstallersCarryVersionedRepairableMigrations(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	packagingDir := filepath.Join(filepath.Dir(sourceFile), "..", "..", "packaging")
	for _, name := range []string{"install-hosted.sh.template", "install-hosted.ps1.template"} {
		contents, err := os.ReadFile(filepath.Join(packagingDir, name))
		if err != nil {
			t.Fatal(err)
		}
		script := string(contents)
		for _, required := range []string{
			"INSTALLATION_MANIFEST_BASE64",
			"installation.json",
			"migrate-config",
			"migration-pairing-url",
			"migration-update-manifest-url",
			"migration-update-public-key",
		} {
			if !strings.Contains(strings.ToUpper(script), strings.ToUpper(required)) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
	}
	linux, err := os.ReadFile(filepath.Join(packagingDir, "install-hosted.sh.template"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(linux), "restore_component") || !strings.Contains(string(linux), "committed=false") {
		t.Fatal("Linux installer does not include transactional rollback")
	}
	windows, err := os.ReadFile(filepath.Join(packagingDir, "install-hosted.ps1.template"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(windows), "} catch {") || !strings.Contains(string(windows), "$backups") {
		t.Fatal("Windows installer does not include repair rollback")
	}
}

func TestHostedInstallersGeneratePollingSchemaAndPreserveExistingEnrollment(t *testing.T) {
	_, sourceFile, _, _ := runtime.Caller(0)
	packagingDir := filepath.Join(filepath.Dir(sourceFile), "..", "..", "packaging")
	for _, fixture := range []struct{ name, schema, preservation string }{
		{"install-hosted.sh.template", `"schemaVersion": 4`, `*[34]'`},
		{"install-hosted.ps1.template", `schemaVersion = 4`, `$existingSchema -eq 3 -or $existingSchema -eq 4`},
	} {
		contents, err := os.ReadFile(filepath.Join(packagingDir, fixture.name))
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range []string{fixture.schema, fixture.preservation, "migrate-config"} {
			if !strings.Contains(string(contents), contract) {
				t.Errorf("%s is missing polling upgrade contract %q", fixture.name, contract)
			}
		}
	}
}
