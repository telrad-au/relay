//go:build !linux

package main

import "time"

func sampleSupportHost(_, _ uint64) (supportSample, uint64, uint64) {
	return supportSample{At: time.Now(), Role: "support-host", Error: "remote_controller_requires_linux"}, 0, 0
}
