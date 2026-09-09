//go:build !relay_container

package main

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

type lifecycleMeasurement struct {
	Step    string    `json:"step"`
	Started time.Time `json:"started"`
	Millis  float64   `json:"millis"`
}

// Measurement is opt-in on the same disposable native hosts used by the real
// installation tests. No payload, identity, private trust, or log is exported.
func measureNativeLifecycle(t *testing.T, p managedPaths) func(string, time.Time) {
	t.Helper()
	output := os.Getenv("TELRAD_PERF_LIFECYCLE_OUT")
	if output == "" {
		return func(string, time.Time) {}
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if os.Getenv("GOCOVERDIR") != "" {
		t.Fatal("native performance evidence requires uninstrumented artifacts")
	}
	type artifact struct {
		Role      string            `json:"role"`
		SHA256    string            `json:"sha256"`
		GoVersion string            `json:"goVersion"`
		Settings  map[string]string `json:"buildSettings"`
	}
	var artifacts []artifact
	for role, path := range map[string]string{"installed": p.Executable, "candidate": os.Getenv("TELRAD_NATIVE_NEXT_BINARY")} {
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		identity := artifact{Role: role, GoVersion: info.GoVersion, Settings: map[string]string{}}
		for _, setting := range info.Settings {
			if (setting.Key == "-race" || setting.Key == "-cover") && setting.Value == "true" {
				t.Fatal("instrumented native candidate cannot establish requirements")
			}
			switch setting.Key {
			case "-race", "-cover", "CGO_ENABLED", "GOOS", "GOARCH", "vcs.revision", "vcs.modified", "-trimpath":
				identity.Settings[setting.Key] = setting.Value
			}
		}
		binary, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.New()
		_, readErr := io.Copy(sum, binary)
		closeErr := binary.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal("native artifact identity unavailable")
		}
		identity.SHA256 = hex.EncodeToString(sum.Sum(nil))
		artifacts = append(artifacts, identity)
	}
	type diskSample struct {
		At             time.Time `json:"at"`
		InstallBytes   int64     `json:"installationBytes"`
		Available      uint64    `json:"diskAvailableBytes"`
		StateAvailable uint64    `json:"stateDiskAvailableBytes"`
	}
	var mu sync.Mutex
	var samples []diskSample
	var steps []lifecycleMeasurement
	stop, done := make(chan struct{}), make(chan struct{})
	sample := func() {
		var size int64
		for directory := range map[string]bool{filepath.Dir(p.Executable): true, filepath.Dir(p.Config): true} {
			err := filepath.WalkDir(directory, func(path string, d fs.DirEntry, err error) error {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if d.Type().IsRegular() {
					info, err := d.Info()
					if err != nil {
						return err
					}
					size += info.Size()
				}
				return nil
			})
			if err != nil {
				t.Error("native installation disk accounting failed")
			}
		}
		stateAvailable, stateErr := performanceDiskAvailable(filepath.Dir(p.Config))
		if stateErr != nil {
			t.Error("native state disk measurement unavailable")
		}
		available, err := performanceDiskAvailable(filepath.Dir(p.Executable))
		if err != nil {
			t.Error("native disk measurement unavailable")
		}
		mu.Lock()
		samples = append(samples, diskSample{time.Now().UTC(), size, available, stateAvailable})
		mu.Unlock()
	}
	sample()
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sample()
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		sample()
		mu.Lock()
		defer mu.Unlock()
		data, err := json.MarshalIndent(map[string]any{"schemaVersion": 1, "platform": runtime.GOOS + "-" + runtime.GOARCH, "goVersion": runtime.Version(), "passed": !t.Failed(), "artifacts": artifacts, "steps": steps, "diskSamples": samples}, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := file.Write(append(data, '\n')); err != nil {
			t.Error(err)
		}
	})
	return func(step string, start time.Time) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, lifecycleMeasurement{step, start.UTC(), float64(time.Since(start)) / 1e6})
	}
}
