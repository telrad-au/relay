package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type supportSample struct {
	At                  time.Time `json:"at"`
	Role                string    `json:"role"`
	CPU                 float64   `json:"cpuPercent"`
	Memory              string    `json:"memory"`
	Network             string    `json:"networkIO"`
	Block               string    `json:"blockIO"`
	PIDs                int       `json:"pids"`
	Saturated           bool      `json:"saturated"`
	Error               string    `json:"error,omitempty"`
	HostMemoryTotal     uint64    `json:"hostMemoryTotal,omitempty"`
	HostMemoryAvailable uint64    `json:"hostMemoryAvailable,omitempty"`
}

func (r *runner) monitorSupport(ctx context.Context, save func(supportSample)) {
	var lastTotal, lastIdle uint64
	for ctx.Err() == nil {
		if r.remoteDocker != "" {
			s, total, idle := sampleSupportHost(lastTotal, lastIdle)
			lastTotal, lastIdle = total, idle
			save(s)
		}
		names, err := r.docker(ctx, "ps", "--filter", "label=telrad.perf="+r.id, "--format", "{{.Names}}")
		if err != nil {
			if ctx.Err() == nil {
				save(supportSample{At: time.Now(), Error: "support_inventory_unavailable"})
			}
			return
		}
		args := []string{"stats", "--no-stream", "--format", "{{json .}}"}
		args = append(args, strings.Fields(string(names))...)
		if len(args) == 4 {
			save(supportSample{At: time.Now(), Error: "support_inventory_empty"})
			return
		}
		output, err := r.docker(ctx, args...)
		if err != nil {
			if ctx.Err() == nil {
				save(supportSample{At: time.Now(), Error: "support_stats_unavailable"})
			}
			return
		}
		var totalCPU float64
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			var row struct{ Name, CPUPerc, MemPerc, MemUsage, NetIO, BlockIO, PIDs string }
			if json.Unmarshal([]byte(line), &row) != nil {
				save(supportSample{At: time.Now(), Error: "invalid_support_stats"})
				continue
			}
			role := strings.TrimPrefix(row.Name, r.id+"-")
			if strings.HasPrefix(role, "relay-") {
				continue
			}
			cpu, err := strconv.ParseFloat(strings.TrimSuffix(row.CPUPerc, "%"), 64)
			if err != nil {
				save(supportSample{At: time.Now(), Error: "invalid_support_cpu"})
				continue
			}
			memory, memoryErr := strconv.ParseFloat(strings.TrimSuffix(row.MemPerc, "%"), 64)
			pids, pidsErr := strconv.Atoi(row.PIDs)
			if pids == 0 && (strings.HasPrefix(role, "traffic-") || strings.HasPrefix(role, "calibration-")) {
				continue
			}
			if memoryErr != nil || pidsErr != nil || pids < 1 || !strings.Contains(row.MemUsage, "/") || !strings.Contains(row.NetIO, "/") || !strings.Contains(row.BlockIO, "/") {
				save(supportSample{At: time.Now(), Role: role, Error: "incomplete_support_metrics"})
				continue
			}
			totalCPU += cpu
			save(supportSample{At: time.Now(), Role: role, CPU: cpu, Memory: row.MemUsage, Network: row.NetIO, Block: row.BlockIO, PIDs: pids, Saturated: cpu > 90*float64(r.hostCPUs) || memory > 85})
		}
		if totalCPU > 90*float64(r.hostCPUs) {
			save(supportSample{At: time.Now(), Role: "support-total", CPU: totalCPU, Saturated: true})
		}
		if !waitUntil(ctx, time.Now().Add(2*time.Second)) {
			return
		}
	}
}

// Only the test helper receives NET_ADMIN. Relay keeps its release capabilities.
// Both egress directions are shaped, and destination filters isolate cloud
// traffic from the clinic-facing listeners and test control channel.
func (r *runner) shapeNetwork(ctx context.Context, relay string) error {
	cloud := ""
	for _, c := range r.containers {
		if strings.HasPrefix(strings.TrimPrefix(c, r.id+"-"), "cloud-") {
			cloud = c
			break
		}
	}
	if cloud == "" {
		return errors.New("cloud container missing")
	}
	ip := func(name string) (string, error) {
		b, err := r.docker(ctx, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
		address := strings.TrimSpace(string(b))
		if net.ParseIP(address) == nil {
			return "", errors.New("container IP unavailable")
		}
		return address, err
	}
	relayIP, err := ip(relay)
	if err != nil {
		return err
	}
	cloudIP, err := ip(cloud)
	if err != nil {
		return err
	}
	const script = `set -eu
tc qdisc replace dev eth0 root handle 1: prio bands 3
tc qdisc replace dev eth0 parent 1:3 handle 30: netem delay "$2" rate "$3" limit 4096
tc filter replace dev eth0 protocol ip parent 1: prio 1 u32 match ip dst "$1/32" flowid 1:3
tc -j qdisc show dev eth0
tc -j filter show dev eth0 parent 1:
`
	for _, direction := range []struct{ source, destination string }{{relay, cloudIP}, {cloud, relayIP}} {
		name, err := r.start(ctx, "shaper", []string{"--network", "container:" + direction.source, "--cap-drop", "ALL", "--cap-add", "NET_ADMIN", "--entrypoint", "sh"}, "telrad-relay-perf:local", []string{"-c", script, "shape", direction.destination, fmt.Sprintf("%gms", r.p.RTT/2), fmt.Sprintf("%gmbit", r.p.Bandwidth)})
		if err != nil {
			return err
		}
		if err := r.waitContainer(ctx, name); err != nil {
			return errors.New("network shaping unavailable")
		}
		b, err := r.docker(ctx, "logs", name)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), `"kind":"netem"`) && !strings.Contains(string(b), `"kind": "netem"`) {
			return errors.New("effective netem settings unavailable")
		}
		if err := os.WriteFile(filepath.Join(r.out, strings.TrimPrefix(name, r.id+"-")+".jsonl"), b, 0600); err != nil {
			return err
		}
	}
	return nil
}

func writeExternalSetup(out string, cfg workerConfig, work string) error {
	// Setup belongs in the private temporary workspace, not the exported report.
	dir := filepath.Join(work, "native-setup")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var configuration map[string]any
	if err := readJSON(filepath.Join(work, "relay-defaults.json"), &configuration); err != nil {
		return err
	}
	configuration["pairingUrl"] = cfg.Origin + "/v1/relay/pairing-enrollments"
	configuration["controlUrl"] = cfg.Origin + "/v1/relay/control"
	configuration["dicomUrl"] = cfg.Origin + "/v1/relay/ingest/dicom"
	configuration["hl7Url"] = cfg.Origin + "/v1/relay/ingest/hl7"
	configuration["relayId"] = "perf-relay"
	configuration["credentialPath"] = "relay-credential.json"
	configuration["hl7MaxBytes"] = cfg.Profile.HL7Limit
	if cfg.Mode == "faults" {
		configuration["dicomIdleTimeoutSeconds"] = 5
		configuration["dicomLifetimeSeconds"] = 15
	}
	host, _, err := net.SplitHostPort(strings.TrimPrefix(cfg.Origin, "https://"))
	if err != nil {
		return err
	}
	configuration["reportHost"] = host
	configuration["reportPort"] = 2576
	if err := writeJSON(filepath.Join(dir, "report-signing-key.json"), syntheticReportKeyRecord(cfg)); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "relay.json"), configuration); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "relay-credential.json"), map[string]any{"schemaVersion": 1, "credential": cfg.Credential}); err != nil {
		return err
	}
	ca, err := os.ReadFile(filepath.Join(work, "ca.pem"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), ca, 0600); err != nil {
		return err
	}
	fmt.Println("Private native setup:", dir)
	return nil
}

// The transparent TLS hop uses the same two shaped cloud queues as Docker Relay.
func (r *runner) shapeExternalNetwork(ctx context.Context) error {
	for _, name := range r.containers {
		if strings.HasPrefix(strings.TrimPrefix(name, r.id+"-"), "proxy-") {
			return r.shapeNetwork(ctx, name)
		}
	}
	return errors.New("external TLS proxy is missing")
}

func (r *runner) supportObservation(ctx context.Context) func() []supportSample {
	monitorCtx, cancel := context.WithCancel(ctx)
	var samples []supportSample
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.monitorSupport(monitorCtx, func(s supportSample) { samples = append(samples, s) })
	}()
	return func() []supportSample { cancel(); <-done; return samples }
}

func (r *runner) monitoredTraffic(ctx context.Context, cfg workerConfig, calibration bool) (trafficResult, []supportSample, error) {
	stop := r.supportObservation(ctx)
	traffic, err := r.traffic(ctx, cfg, calibration)
	return traffic, stop(), err
}
