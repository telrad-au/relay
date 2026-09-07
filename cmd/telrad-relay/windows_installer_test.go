package main

import (
	"os"
	"strings"
	"testing"
)

func TestWindowsInstallersDelegateProtectedMutations(t *testing.T) {
	for _, name := range []string{"install.ps1", "install-hosted.ps1.template"} {
		data, err := os.ReadFile("../../packaging/" + name)
		if err != nil {
			t.Fatal(err)
		}
		script := string(data)
		for _, required := range []string{"install-native", "clinicRemoteAddress", "installation", "ConvertTo-Json", "ProgramFiles", "Process"} {
			if !strings.Contains(script, required) {
				t.Errorf("%s missing %s", name, required)
			}
		}
		for _, forbidden := range []string{"chown -R", "Copy-Item $download $binary", "sc.exe config", "Set-TelradDirectoryAcl", "$env:ProgramData"} {
			if strings.Contains(script, forbidden) {
				t.Errorf("%s still mutates service-controlled paths in the script", name)
			}
		}
	}
}
