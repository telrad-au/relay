//go:build linux && !relay_container

package main

import (
	"testing"
)

func TestLinuxNativeActionRejectsGeneralCLIArguments(t *testing.T) {
	for _, args := range [][]string{{"--config", "/tmp/relay.json", "auth"}, {"auth", "--config", "/tmp/relay.json"}, {"status"}, {"update"}, {"update", "1.2.3", "../../target"}, {"start", "other.service"}} {
		if err := validateNativeAction(args); err == nil {
			t.Fatalf("unsafe action accepted: %q", args)
		}
	}
}
