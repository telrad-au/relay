package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func (r *runner) startObserver(ctx context.Context, relay, role string) (string, error) {
	return r.start(ctx, role, []string{"--user", "10001:10001", "--pid", "container:" + relay, "--cgroupns", "host", "--mount", "type=bind,src=/sys/fs/cgroup,dst=/host-cgroup,readonly"}, "telrad-relay-perf:local", []string{"observe"})
}

func (r *runner) collectObserver(ctx context.Context, name string) ([]sample, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := r.docker(ctx, "stop", "--time", "5", name); err != nil {
		return nil, err
	}
	b, err := r.docker(ctx, "logs", name)
	if err != nil {
		return nil, err
	}
	var samples []sample
	scanner := bufio.NewScanner(strings.NewReader(string(b)))
	for scanner.Scan() {
		var s sample
		if json.Unmarshal(scanner.Bytes(), &s) != nil {
			return samples, errors.New("invalid observer output")
		}
		samples = append(samples, s)
	}
	return samples, scanner.Err()
}
