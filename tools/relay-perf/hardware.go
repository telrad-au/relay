package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func hostInfo(ctx context.Context) (map[string]any, error) {
	info := map[string]any{"os": runtime.GOOS, "architecture": runtime.GOARCH, "logicalCPUs": runtime.NumCPU(), "goVersion": runtime.Version()}
	if runtime.GOOS == "windows" {
		b, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$p=Get-CimInstance Win32_Processor; $o=Get-CimInstance Win32_OperatingSystem; @{processor=($p.Name -join '; ');osVersion=$o.Version;osCaption=$o.Caption;machineRamBytes=([uint64]$o.TotalVisibleMemorySize*1024)} | ConvertTo-Json -Compress`).Output()
		if err != nil {
			return info, errors.New("native hardware identity unavailable")
		}
		var native map[string]any
		if json.Unmarshal(b, &native) != nil {
			return info, errors.New("invalid native hardware identity")
		}
		for k, v := range native {
			info[k] = v
		}
		return info, nil
	}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return info, errors.New("hardware identity requires Linux or Windows")
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && (strings.TrimSpace(parts[0]) == "model name" || strings.TrimSpace(parts[0]) == "Hardware") {
			info["processor"] = strings.TrimSpace(parts[1])
			break
		}
	}
	if kernel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		info["kernel"] = strings.TrimSpace(string(kernel))
	}
	if osRelease, err := os.ReadFile("/etc/os-release"); err == nil {
		info["userspaceOSRelease"] = string(osRelease)
	}
	if _, ok := info["processor"]; !ok {
		return info, errors.New("processor model unavailable")
	}
	return info, nil
}
