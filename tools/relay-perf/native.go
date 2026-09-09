package main

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"
)

type nativeMetadata struct {
	PID          int               `json:"pid"`
	At           time.Time         `json:"at"`
	Hardware     map[string]any    `json:"hardware"`
	BinarySHA256 string            `json:"binarySha256"`
	GoVersion    string            `json:"goVersion"`
	Settings     map[string]string `json:"buildSettings"`
}

func nativeIdentity(ctx context.Context, pid int) (nativeMetadata, error) {
	m := nativeMetadata{PID: pid, At: time.Now().UTC(), Settings: map[string]string{}}
	path, err := nativeExecutable(ctx, pid)
	if err != nil {
		return m, err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return m, errors.New("native executable build identity unavailable")
	}
	if info.Path != "github.com/telrad-au/relay/cmd/telrad-relay" {
		return m, errors.New("PID does not identify a Relay executable")
	}
	m.GoVersion = info.GoVersion
	for _, s := range info.Settings {
		switch s.Key {
		case "-race", "-cover", "CGO_ENABLED", "GOOS", "GOARCH", "vcs.revision", "vcs.modified", "-trimpath":
			m.Settings[s.Key] = s.Value
		}
	}
	if m.Settings["-race"] == "true" || m.Settings["-cover"] == "true" {
		return m, errors.New("native qualification requires an uninstrumented release executable")
	}
	file, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return m, err
	}
	m.BinarySHA256 = hex.EncodeToString(sum.Sum(nil))
	m.Hardware, err = hostInfo(ctx)
	return m, err
}
