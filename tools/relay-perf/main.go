// relay-perf is synthetic test infrastructure, never part of Relay's release.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := command(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: relay-perf screen|smoke|sweep|qualify|experiment|fault-check|external|external-fault-check|observe-native [options]")
	}
	mode := args[0]
	f := flag.NewFlagSet(mode, flag.ContinueOnError)
	defaultProfile := "clinic-v1"
	if mode == "screen" {
		defaultProfile = "clinic-mixed-v1"
	}
	profileName := f.String("profile", defaultProfile, "built-in profile name or JSON file")
	cpus := f.Float64("cpus", 1, "Relay CPU quota")
	memory := f.String("memory", "256MiB", "Relay hard memory limit; swap disabled")
	out := f.String("out", "dist/perf", "new result directory; existing output is never overwritten")
	caseName := f.String("case", "", "experiment name")
	dir := f.String("work", "/work", "private worker configuration directory")
	relay := f.String("relay-host", "", "native Relay host for external mode")
	remoteDocker := f.String("relay-docker", "", "SSH Docker endpoint for a separately hosted screening target")
	support := f.String("support-host", "", "support host address reachable by the native service")
	pid := f.Int("pid", 0, "native Relay process ID to observe")
	duration := f.Duration("duration", 0, "short screening/experiment duration; disallowed for qualification")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if mode == "proxy" {
		return serveProxy(ctx)
	}
	if mode == "host-info" {
		info, err := hostInfo(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(info)
	}
	if mode == "observe-native" {
		if *pid < 1 || *duration <= 0 {
			return errors.New("observe-native requires --pid and --duration")
		}
		file, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		metadata, err := nativeIdentity(ctx, *pid)
		if err != nil {
			return err
		}
		if err := writeJSON(*out+".metadata.json", metadata); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, *duration)
		defer cancel()
		return observe(ctx, file, *pid, true)
	}
	if mode == "cloud" || mode == "ris" || mode == "traffic" || mode == "faults" || mode == "init" || mode == "observe" {
		if mode == "observe" {
			return observe(ctx, os.Stdout, 1, false)
		}
		cfg, err := readWorker(*dir)
		if err != nil {
			return errors.New("worker configuration unavailable")
		}
		switch mode {
		case "cloud":
			return serveCloud(ctx, cfg, *dir)
		case "ris":
			return serveRIS(ctx, cfg)
		case "traffic":
			limit := cfg.Profile.total() + 90*time.Second
			if cfg.Calibration {
				limit = seconds(cfg.Profile.calibrationSeconds()) + 90*time.Second
			}
			ctx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()
			return trafficToFile(ctx, cfg, *dir, *out)
		case "faults":
			return writeJSON(*out, runFaults(ctx, cfg, *dir))
		case "init":
			return initializeRelayState(cfg, *dir, "/relaystate")
		}
	}
	if mode != "screen" && mode != "smoke" && mode != "sweep" && mode != "qualify" && mode != "experiment" && mode != "external" && mode != "fault-check" && mode != "external-fault-check" {
		return errors.New("unknown command")
	}
	p, err := loadProfile(*profileName)
	if err != nil {
		return err
	}
	if mode == "experiment" {
		p, err = experimentProfile(p, *caseName)
		if err != nil {
			return err
		}
	}
	if *cpus <= 0 || *cpus > 64 || math.IsNaN(*cpus) || math.IsInf(*cpus, 0) {
		return errors.New("cpus must be greater than zero and at most 64")
	}
	mem, err := memoryBytes(*memory)
	if err != nil {
		return err
	}
	if *duration < 0 {
		return errors.New("duration cannot be negative")
	}
	if *remoteDocker != "" {
		if mode != "screen" || *relay == "" || *support == "" {
			return errors.New("relay-docker requires screen, relay-host and support-host")
		}
		if err := validateRemoteDocker(*remoteDocker, *relay, *support); err != nil {
			return err
		}
	}
	if mode == "qualify" || mode == "external" {
		if *duration != 0 || p.Repetitions < 3 || p.total() < time.Hour {
			return errors.New("qualification requires at least three full one-hour repetitions")
		}
	} else if mode == "screen" {
		p.Repetitions = 1
		if *duration > 0 {
			p.Nominal = duration.Seconds()
			p.Headroom = p.Nominal / 5
		}
	} else {
		p.Repetitions = 1
		p.Idle = 5
		p.Nominal = 45
		p.Headroom = 15
		p.Recovery = 15
		if mode == "sweep" || mode == "experiment" {
			p.Idle = 10
			p.Nominal = 240
			p.Headroom = 60
			p.Recovery = 30
		}
		if *duration > 0 {
			p.Nominal = duration.Seconds()
			p.Headroom = max(1, p.Nominal/4)
		}
	}
	if err := p.validate(); err != nil {
		return err
	}
	if (mode == "external" || mode == "external-fault-check") && (*relay == "" || *support == "") {
		return errors.New("external requires --relay-host and --support-host")
	}
	if mode == "external" || mode == "external-fault-check" {
		if net.ParseIP(*relay).To4() == nil || net.ParseIP(*support).To4() == nil {
			return errors.New("external test networking requires native and support IPv4 addresses")
		}
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "packaging/relay.example.json")); err != nil {
		return errors.New("run from the Relay repository root")
	}
	output, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if _, err := os.Stat(output); err == nil {
		return errors.New("output already exists; choose a new --out directory")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	points := []struct {
		CPU    float64
		Memory int64
	}{{*cpus, mem}}
	if mode == "sweep" {
		points = []struct {
			CPU    float64
			Memory int64
		}{{2, 512 << 20}, {1, 512 << 20}, {1, 256 << 20}, {1, 128 << 20}, {1, 64 << 20}}
	}
	allPassed := true
	for i, point := range points {
		for repetition := 1; repetition <= p.Repetitions; repetition++ {
			path := filepath.Join(output, fmt.Sprintf("point-%d-run-%d", i+1, repetition))
			r := &runner{root: root, out: path, p: p, mode: mode, cpus: point.CPU, memory: point.Memory, externalRelay: *relay, supportHost: *support, remoteDocker: *remoteDocker}
			res, err := r.run(ctx)
			fmt.Printf("%s: %s (%s CPUs, %d MiB)\n", filepath.Base(path), res.Status, strconv.FormatFloat(point.CPU, 'g', -1, 64), point.Memory>>20)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			if res.Status != "PASS" {
				allPassed = false
				if mode == "sweep" {
					return errors.New("screening stopped at the first non-passing resource point")
				}
			}
		}
	}
	if !allPassed {
		return errors.New("performance run did not pass; inspect result.json in the output directory")
	}
	return nil
}
