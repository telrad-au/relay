package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// Only the isolated test container is restarted with shortened fault timeouts.
// Nominal measurements and their resource samples have already been captured.
func (r *runner) prepareFaultRuntime(ctx context.Context) error {
	relay := ""
	for _, c := range r.containers {
		if strings.HasPrefix(strings.TrimPrefix(c, r.id+"-"), "relay-") {
			relay = c
			break
		}
	}
	if relay == "" {
		return errors.New("fault target is missing")
	}
	if _, err := r.docker(ctx, "stop", "--time", "30", relay); err != nil {
		return err
	}
	cfg, err := readWorker(r.work)
	if err != nil {
		return err
	}
	cfg.Mode = "faults"
	if err := writeJSON(filepath.Join(r.work, "worker.json"), cfg); err != nil {
		return err
	}
	init, err := r.start(ctx, "fault-init", []string{"--mount", "type=bind,src=" + r.work + ",dst=/work,readonly", "--mount", "type=volume,src=" + r.volume + ",dst=/relaystate"}, "telrad-relay-perf:local", []string{"init"})
	if err != nil {
		return err
	}
	if err := r.waitContainer(ctx, init); err != nil {
		return err
	}
	if _, err := r.docker(ctx, "start", relay); err != nil {
		return err
	}
	if err := r.shapeNetwork(ctx, relay); err != nil {
		return err
	}
	return writeJSON(filepath.Join(r.out, "fault-profile.json"), map[string]int{"dicomIdleTimeoutSeconds": 5, "dicomLifetimeSeconds": 15, "recoverySlackSeconds": 5})
}
