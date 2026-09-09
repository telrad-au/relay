package main

import "time"

func sampleSupportHost(lastTotal, lastIdle uint64) (supportSample, uint64, uint64) {
	s := supportSample{At: time.Now(), Role: "support-host"}
	total, idle, cpuErr := hostCPU()
	memory, memoryErr := numbers("/proc/meminfo")
	if cpuErr != nil || memoryErr != nil || memory["MemTotal"] == 0 || memory["MemAvailable"] == 0 {
		s.Error = "support_host_metrics_unavailable"
		return s, total, idle
	}
	s.HostMemoryTotal, s.HostMemoryAvailable = memory["MemTotal"], memory["MemAvailable"]
	if lastTotal != 0 && total > lastTotal && idle >= lastIdle {
		s.CPU = 100 * (1 - float64(idle-lastIdle)/float64(total-lastTotal))
	}
	s.Saturated = s.CPU > 90 || s.HostMemoryAvailable < s.HostMemoryTotal/10
	return s, total, idle
}
