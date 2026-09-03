package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsInstallersEnforceServiceBoundary(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	packagingDir := filepath.Join(filepath.Dir(sourceFile), "..", "..", "packaging")
	for _, name := range []string{"install.ps1", "install-hosted.ps1.template"} {
		contents, err := os.ReadFile(filepath.Join(packagingDir, name))
		if err != nil {
			t.Fatal(err)
		}
		script := string(contents)
		for _, required := range []string{
			"NT SERVICE\\",
			"sidtype",
			"icacls.exe",
			"/inheritance:r",
			"New-NetFirewallRule",
			"-RemoteAddress $ClinicRemoteAddress",
			"-Profile Domain,Private",
			"New-EventLog -LogName Application",
			`[Environment]::SetEnvironmentVariable("Path", $updatedProcessPath, "Process")`,
		} {
			if !strings.Contains(script, required) {
				t.Errorf("%s does not contain %q", name, required)
			}
		}
		if !strings.Contains(script, ":(OI)(CI)RX") && !strings.Contains(script, `ServiceAccess "RX"`) {
			t.Errorf("%s does not limit the service identity to read and execute access on the install directory", name)
		}
		if !strings.Contains(script, "update-trust.json") {
			t.Errorf("%s does not install an administrator-owned update trust policy", name)
		}
		if name == "install.ps1" {
			for _, required := range []string{
				"$serviceWasRunning",
				"Stop-Service $serviceName",
				"Start-Service $serviceName",
				"Existing authentication preserved.",
				"The service remains stopped. Run 'telrad' to start it.",
			} {
				if !strings.Contains(script, required) {
					t.Errorf("%s does not contain state-aware update behavior %q", name, required)
				}
			}
		}
	}
}
