package main

import (
	"os"
	"strings"
	"testing"
)

func TestAllInstallersUseOneNativeInstallationBoundary(t *testing.T) {
	for _, name := range []string{"install.sh", "install.ps1", "install-hosted.sh.template", "install-hosted.ps1.template"} {
		data, err := os.ReadFile("../../packaging/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"install-native", "config", "trust", "installation"} {
			if !strings.Contains(strings.ToLower(string(data)), required) {
				t.Errorf("%s missing %s", name, required)
			}
		}
		for _, forbidden := range []string{"chown -R", "restore_component", "$backups[", "rm -f /var/lib/telrad-relay"} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("%s contains path-based privileged mutation %q", name, forbidden)
			}
		}
	}
}
