package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAutomaticReportAuthoritySurvivesRestartWithoutConfiguration(t *testing.T) {
	cfg := pairedTestConfig(t.TempDir())
	if err := os.Chmod(reportKeyDirectory(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	saveRetrievalTestConfig(t, cfg)
	if err := ensureReportSigningKey(cfg); err != nil {
		t.Fatal(err)
	}
	grants, err := signReportAuthorizations(cfg, retrievalTestHL7("NW", 1))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(reportKeyDirectory(cfg), reportKeyFilename))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := loadConfigMode(cfg.configPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureReportSigningKey(restarted); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(reportKeyDirectory(cfg), reportKeyFilename))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("restart replaced order authority")
	}
	if err := authorizeReport(restarted, reportTestMessage(reportTestPayload("ACC"), grants[0])); err != nil {
		t.Fatal(err)
	}
	restarted.RelayID = "different-relay"
	restarted.configPath = ""
	if authorizeReport(restarted, reportTestMessage(reportTestPayload("ACC"), grants[0])) == nil {
		t.Fatal("re-pairing reused another Relay's grant")
	}
}

func TestAutomaticReportKeyRefusesUnsafeExistingState(t *testing.T) {
	for _, kind := range []string{"corrupt", "symlink", "hardlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			if runtime.GOOS == "windows" && (kind == "symlink" || kind == "permissions") {
				t.Skip("Unix link/mode fixture; Windows ACL coverage uses native integration")
			}
			cfg := pairedTestConfig(t.TempDir())
			if err := os.Chmod(reportKeyDirectory(cfg), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(reportKeyDirectory(cfg), reportKeyFilename)
			if kind == "permissions" {
				if err := ensureReportSigningKey(cfg); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			} else if kind == "corrupt" {
				if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				target := filepath.Join(t.TempDir(), "unrelated")
				if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(target, path)
				} else {
					err = os.Link(target, path)
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					data, err := os.ReadFile(target)
					if err != nil || string(data) != "untouched" {
						t.Error("key provisioning modified unrelated state")
					}
				})
			}
			if err := ensureReportSigningKey(cfg); err == nil {
				t.Fatal("unsafe key was repaired or replaced")
			}
		})
	}
}
