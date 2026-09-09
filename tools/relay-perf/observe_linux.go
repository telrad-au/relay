//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func nativeExecutable(_ context.Context, pid int) (string, error) {
	return fmt.Sprintf("/proc/%d/exe", pid), nil
}

func numbers(path string) (map[string]uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 2 && fields[2] == "kB" {
			n *= 1024
		}
		out[strings.TrimSuffix(fields[0], ":")] = n
	}
	return out, nil
}

func scalar(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func hostCPU() (uint64, uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	if !s.Scan() {
		return 0, 0, errors.New("missing CPU counters")
	}
	fields := strings.Fields(s.Text())
	if len(fields) < 5 {
		return 0, 0, errors.New("invalid CPU counters")
	}
	var total, idle uint64
	for i, v := range fields[1:min(len(fields), 9)] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		total += n
		if i == 3 {
			idle = n
		}
	}
	return total, idle, nil
}

// This observer runs outside Relay's cgroup, sharing only its PID namespace and
// a read-only host cgroup mount. PID 1 is the uninstrumented release executable.
func observe(ctx context.Context, w io.Writer, pid int, native bool) error {
	process := fmt.Sprintf("/proc/%d", pid)
	var ticks uint64
	if native {
		b, err := exec.CommandContext(ctx, "getconf", "CLK_TCK").Output()
		if err != nil {
			return errors.New("native clock tick frequency unavailable")
		}
		ticks, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || ticks == 0 {
			return errors.New("invalid native clock tick frequency")
		}
	}
	cgroup := ""
	if !native {
		b, err := os.ReadFile(filepath.Join(process, "cgroup"))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "0::/") {
				cgroup = filepath.Join("/host-cgroup", strings.TrimPrefix(line, "0::/"))
				break
			}
		}
		if cgroup == "" {
			return errors.New("qualification requires readable cgroup v2")
		}
	}
	lastTotal, lastIdle, _ := hostCPU()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	encoder := json.NewEncoder(w)
	for {
		s := sample{At: time.Now().UTC(), HostIdle: 100}
		proc, err := numbers(filepath.Join(process, "status"))
		if err != nil {
			s.Error = "process_metrics_unavailable"
		} else {
			s.RSS, s.PeakRSS = proc["VmRSS"], proc["VmHWM"]
		}
		host, err := numbers("/proc/meminfo")
		if err != nil {
			s.Error = "host_metrics_unavailable"
		} else {
			s.HostAvailable, s.HostTotal = host["MemAvailable"], host["MemTotal"]
		}
		total, idle, err := hostCPU()
		if err != nil {
			s.Error = "host_cpu_unavailable"
		} else {
			if total > lastTotal {
				s.HostIdle = 100 * float64(idle-lastIdle) / float64(total-lastTotal)
			}
			lastTotal, lastIdle = total, idle
		}
		ioStats, err := numbers(filepath.Join(process, "io"))
		if err == nil {
			s.ReadBytes, s.WriteBytes = ioStats["read_bytes"], ioStats["write_bytes"]
		} else {
			s.Error = "process_io_unavailable"
		}
		if !native {
			for name, target := range map[string]*uint64{"memory.current": &s.Memory, "memory.peak": &s.Peak, "memory.max": &s.Limit, "memory.swap.max": &s.SwapLimit} {
				n, err := scalar(filepath.Join(cgroup, name))
				if err != nil {
					s.Error = "cgroup_limits_unavailable"
				} else {
					*target = n
				}
			}
			cpu, err := os.ReadFile(filepath.Join(cgroup, "cpu.max"))
			var quota, period float64
			if err != nil {
				s.Error = "cpu_limit_unavailable"
			} else if _, err := fmt.Sscanf(string(cpu), "%f %f", &quota, &period); err != nil || period <= 0 {
				s.Error = "cpu_limit_unavailable"
			} else {
				s.CPUQuota = quota / period
			}
			stats, err := numbers(filepath.Join(cgroup, "cpu.stat"))
			if err != nil {
				s.Error = "cpu_metrics_unavailable"
			} else {
				s.CPUUse, s.Throttled = stats["usage_usec"], stats["throttled_usec"]
			}
			events, err := numbers(filepath.Join(cgroup, "memory.events"))
			if err != nil {
				s.Error = "oom_metrics_unavailable"
			} else {
				s.OOM = events["oom_kill"]
			}
		} else {
			// /proc/PID/stat accounts for all threads in the process. schedstat
			// would measure only the main OS thread of a multi-threaded Go runtime.
			b, err := os.ReadFile(filepath.Join(process, "stat"))
			if err != nil {
				s.Error = "native_cpu_unavailable"
			} else {
				end := strings.LastIndexByte(string(b), ')')
				if end < 0 {
					s.Error = "native_cpu_unavailable"
				} else {
					fields := strings.Fields(string(b)[end+1:])
					if len(fields) < 13 {
						s.Error = "native_cpu_unavailable"
					} else {
						user, err1 := strconv.ParseUint(fields[11], 10, 64)
						system, err2 := strconv.ParseUint(fields[12], 10, 64)
						if err1 != nil || err2 != nil {
							s.Error = "native_cpu_unavailable"
						} else {
							s.CPUUse = (user + system) * 1000000 / ticks
						}
					}
				}
			}
		}
		if err := encoder.Encode(s); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
