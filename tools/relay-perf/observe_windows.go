//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

func nativeExecutable(ctx context.Context, pid int) (string, error) {
	b, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Get-Process -Id "+strconv.Itoa(pid)+").Path").Output()
	if err != nil {
		return "", errors.New("native process path unavailable")
	}
	return strings.TrimSpace(string(b)), nil
}

func observe(ctx context.Context, w io.Writer, pid int, native bool) error {
	if !native || pid < 1 {
		return errors.New("Windows observation requires a native process ID")
	}
	// Fixed script and a validated numeric PID; no shell interpolation of paths,
	// credentials, user input, or clinical data.
	script := `$ErrorActionPreference='Stop'; $relayProcessId=` + strconv.Itoa(pid) + `; while($true) {
 $p=Get-Process -Id $relayProcessId
 $io=Get-CimInstance Win32_Process -Filter ("ProcessId="+$relayProcessId)
 $os=Get-CimInstance Win32_OperatingSystem
 $cpu=Get-CimInstance Win32_PerfFormattedData_PerfOS_Processor -Filter "Name='_Total'"
 @{at=[DateTime]::UtcNow.ToString('o');readBytes=[uint64]$io.ReadTransferCount;writeBytes=[uint64]$io.WriteTransferCount;rssBytes=$p.WorkingSet64;peakRssBytes=$p.PeakWorkingSet64;cpuUseMicros=($p.TotalProcessorTime.Ticks/10);hostTotalBytes=([uint64]$os.TotalVisibleMemorySize*1024);hostAvailableBytes=([uint64]$os.FreePhysicalMemory*1024);hostIdlePercent=$cpu.PercentIdleTime} | ConvertTo-Json -Compress
 Start-Sleep -Seconds 1
}`
	c := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	c.Stdout = w
	c.Stderr = io.Discard
	if err := c.Run(); err != nil && ctx.Err() == nil {
		return errors.New("native Windows observation failed")
	}
	return nil
}
