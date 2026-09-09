package main

import (
	"context"
	"fmt"
	"path/filepath"
)

func fixturePath(cfg workerConfig, work string, f fixture) string {
	if cfg.FixtureDirectory != "" {
		work = cfg.FixtureDirectory
	}
	return filepath.Join(work, f.File)
}

func (r *runner) fixtureMount() []string {
	if r.fixtureVolume == "" {
		return nil
	}
	return []string{"--mount", "type=volume,src=" + r.fixtureVolume + ",dst=/fixtures,readonly"}
}

// Stage only generated fixture files after independent validation. Copy/remove
// one file at a time so staging does not require a second whole-corpus copy.
// Docker's native filesystem avoids macOS/Windows bind-mount streaming overhead.
func (r *runner) stageFixtureVolume(ctx context.Context) error {
	fmt.Println("Staging validated fixtures on Docker storage before measurement...")
	r.fixtureVolume = r.id + "-fixtures"
	if _, err := r.docker(ctx, "volume", "create", "--label", "telrad.perf="+r.id, r.fixtureVolume); err != nil {
		return err
	}
	const script = `set -eu
for file in /work/fixture-*.dcm; do
    cp "$file" /fixtures/
    rm "$file"
done
`
	name, err := r.start(ctx, "fixture-copy", []string{"--entrypoint", "sh", "--mount", "type=bind,src=" + r.work + ",dst=/work", "--mount", "type=volume,src=" + r.fixtureVolume + ",dst=/fixtures"}, "telrad-relay-perf:local", []string{"-c", script})
	if err != nil {
		return err
	}
	return r.waitContainer(ctx, name)
}
