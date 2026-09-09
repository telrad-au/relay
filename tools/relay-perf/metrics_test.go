package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func passingResult(t *testing.T) result {
	t.Helper()
	p, err := loadProfile("clinic-v1")
	if err != nil {
		t.Fatal(err)
	}
	p.Idle, p.Nominal, p.Headroom, p.Recovery = 0, 1, 0, 1
	p.Instances = 8
	r := result{Mode: "smoke", Profile: p, Start: time.Now(), Supporting: []supportSample{{Role: "cloud"}, {Role: "cloud"}, {Role: "ris"}, {Role: "ris"}, {Role: "traffic"}, {Role: "traffic"}}}
	r.CalibrationSupporting = []supportSample{{Role: "cloud"}, {Role: "cloud"}, {Role: "ris"}, {Role: "ris"}, {Role: "calibration"}, {Role: "calibration"}}
	for _, kind := range []string{"dicom", "hl7", "report"} {
		for _, e := range planned(p, r.Start, kind, false) {
			e.Started = e.Scheduled
			e.Finished = e.Scheduled.Add(time.Millisecond)
			e.Bytes = 1024
			r.Events = append(r.Events, e)
		}
		for _, e := range planned(p, r.Start, kind, true) {
			e.Started = e.Scheduled
			e.Finished = e.Scheduled.Add(time.Millisecond)
			r.Calibration = append(r.Calibration, e)
		}
	}
	r.Events = append(r.Events, studyEvents(r.Events, p.Instances)...)
	r.Cloud = cloudSnapshot{DICOMReceipts: 8, HL7Accepted: 1, ReportsConfirmed: 1}
	for i := 0; i < 2; i++ {
		r.Samples = append(r.Samples, sample{At: r.Start.Add(time.Duration(i) * time.Second), Limit: 256 << 20, CPUQuota: 1, Peak: 32 << 20, RSS: 24 << 20, HostTotal: 4 << 30, HostAvailable: 2 << 30, HostIdle: 50})
	}
	return r
}

func TestVerdictRejectsFalsePerformanceSuccess(t *testing.T) {
	for _, test := range []struct {
		name, want string
		mutate     func(*result)
	}{
		{"valid", "PASS", func(*result) {}},
		{"payload-corruption", "FAIL", func(r *result) { r.Cloud.Errors++ }},
		{"early-success", "FAIL", func(r *result) { r.Cloud.DICOMReceipts-- }},
		{"missed-offered-work", "FAIL", func(r *result) { r.Events = r.Events[1:]; r.Cloud.DICOMReceipts-- }},
		{"duplicate-offered-work", "FAIL", func(r *result) { r.Events[1] = r.Events[0] }},
		{"changed-schedule", "FAIL", func(r *result) { r.Events[0].Scheduled = r.Events[0].Scheduled.Add(time.Second) }},
		{"sender-backlog", "FAIL", func(r *result) { r.Events[0].Finished = time.Time{} }},
		{"sender-drop", "FAIL", func(r *result) { r.Events[0].Error = "offered_queue_full" }},
		{"oom", "FAIL", func(r *result) { r.Samples[0].OOM = 1 }},
		{"swap-enabled", "INCONCLUSIVE", func(r *result) { r.Samples[0].SwapLimit = 256 << 20 }},
		{"cpu-not-enforced", "INCONCLUSIVE", func(r *result) { r.Samples[0].CPUQuota = 2 }},
		{"memory-not-enforced", "INCONCLUSIVE", func(r *result) { r.Samples[0].Limit = 512 << 20 }},
		{"missing-samples", "INCONCLUSIVE", func(r *result) { r.Samples = nil }},
		{"missing-ris-observation", "INCONCLUSIVE", func(r *result) { r.Supporting = r.Supporting[:2] }},
		{"receiver-bottleneck", "INCONCLUSIVE", func(r *result) { r.Supporting[0].Saturated = true }},
		{"latency-with-support-saturation", "INCONCLUSIVE", func(r *result) {
			r.Mode = "sweep"
			r.Events[0].Finished = r.Events[0].Scheduled.Add(3 * time.Second)
			r.Supporting[0].Saturated = true
		}},
		{"host-bottleneck", "INCONCLUSIVE", func(r *result) {
			s := r.Samples[0]
			s.HostIdle = 1
			r.Samples = nil
			for range 10 {
				r.Samples = append(r.Samples, s)
			}
		}},
		{"missing-calibration-metrics", "INCONCLUSIVE", func(r *result) { r.CalibrationSupporting = nil }},
		{"failed-calibration", "INCONCLUSIVE", func(r *result) { r.Calibration = r.Calibration[1:] }},
		{"cancelled", "FAIL", func(r *result) { r.Events[0].Error = "cancelled_before_start" }},
		{"cleanup-failed", "FAIL", func(r *result) { r.CleanupErrors = []string{"failed"} }},
		{"fault-leak", "FAIL", func(r *result) { r.Faults = []faultResult{{Name: "abort", Passed: false}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := passingResult(t)
			test.mutate(&r)
			evaluate(&r, 1, 256<<20)
			if r.Status != test.want {
				t.Fatalf("status=%s reasons=%v", r.Status, r.Reasons)
			}
		})
	}
}

func TestScheduleCountsBlockedAndCancelledArrivals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var log eventLog
	var completed atomic.Int32
	var arrivals []event
	for i := 0; i < 2000; i++ {
		arrivals = append(arrivals, event{Kind: "dicom", Sequence: i, Scheduled: time.Now()})
	}
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		schedule(ctx, arrivals, 1, func(ctx context.Context, e event) event {
			if completed.Add(1) == 1 {
				close(started)
			}
			<-ctx.Done()
			e.Error = "cancelled"
			e.Finished = time.Now()
			return e
		}, &log)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler failed to cancel")
	}
	got := log.snapshot()
	if len(got) != len(arrivals) {
		t.Fatalf("recorded %d of %d offered operations", len(got), len(arrivals))
	}
	for _, e := range got {
		if e.Error == "" {
			t.Fatal("blocked work was recorded as success")
		}
	}
}

func TestLatencyIncludesOfferedQueueTime(t *testing.T) {
	now := time.Now()
	s := summarize([]event{{Kind: "hl7", Scheduled: now, Started: now.Add(4 * time.Second), Finished: now.Add(5 * time.Second)}}, 10)["hl7"]
	if s.P99 != 5000 || s.Queue.P99 != 4000 || s.Transfer.P99 != 1000 || s.PerSecond != .1 {
		t.Fatalf("queue delay missing: %+v", s)
	}
}

func TestProfileRejectsUnsafeOrAmbiguousBounds(t *testing.T) {
	p, err := loadProfile("clinic-v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*profile){func(p *profile) { p.Rows = 65536 }, func(p *profile) { p.HL7Connections = 129 }, func(p *profile) { p.HL7Bytes = 9 << 20 }, func(p *profile) { p.PDU = 1 << 20 }, func(p *profile) { p.Factor = 1 }, func(p *profile) { p.Recovery = 0 }} {
		copy := p
		mutate(&copy)
		if copy.validate() == nil {
			t.Fatal("invalid profile accepted")
		}
	}
	if _, err := memoryBytes("256"); err == nil {
		t.Fatal("unitless memory accepted")
	}
	for _, v := range []string{"256MiB", "1GiB"} {
		if _, err := memoryBytes(v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommandDoesNotOverwriteOrShortenQualification(t *testing.T) {
	if err := command(context.Background(), []string{"qualify", "--duration", "1s"}); err == nil || !strings.Contains(err.Error(), "three full") {
		t.Fatalf("shortened qualification accepted: %v", err)
	}
	if err := command(context.Background(), []string{"observe-native", "--pid", "-1", "--duration", "1s"}); err == nil {
		t.Fatal("invalid PID accepted")
	}
}

func TestQualificationRequiresStableMemoryAndCompleteFaultObservation(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func(*result)
	}{
		{"valid", "PASS", func(*result) {}},
		{"peak-headroom", "FAIL", func(r *result) { r.Samples[0].Peak = 205 << 20 }},
		{"resident-growth", "FAIL", func(r *result) {
			for i := range r.Samples {
				if r.Samples[i].At.Sub(r.Start) > 2100*time.Second {
					r.Samples[i].RSS = 40 << 20
				}
			}
		}},
		{"observation-gap", "INCONCLUSIVE", func(r *result) { r.Samples = append(r.Samples[:1600], r.Samples[1610:]...) }},
		{"supporting-observation-gap", "INCONCLUSIVE", func(r *result) { r.Supporting = append(r.Supporting[:300], r.Supporting[360:]...) }},
		{"calibration-observation-gap", "INCONCLUSIVE", func(r *result) { r.CalibrationSupporting = r.CalibrationSupporting[:6] }},
		{"missing-fault-case", "INCONCLUSIVE", func(r *result) { r.Faults = r.Faults[1:] }},
		{"missing-fault-resources", "INCONCLUSIVE", func(r *result) { r.FaultSamples = nil }},
		{"fault-observation-gap", "INCONCLUSIVE", func(r *result) { r.FaultSamples = r.FaultSamples[:2] }},
		{"fault-support-observation-gap", "INCONCLUSIVE", func(r *result) { r.FaultSupporting = r.FaultSupporting[:6] }},
		{"fault-oom", "FAIL", func(r *result) { r.FaultSamples[0].OOM = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := passingResult(t)
			p, err := loadProfile("clinic-v1")
			if err != nil {
				t.Fatal(err)
			}
			r.Mode, r.Profile = "qualify", p
			r.Events = nil
			counts := map[string]int{}
			for _, kind := range []string{"dicom", "hl7", "report"} {
				for _, e := range planned(p, r.Start, kind, false) {
					e.Started = e.Scheduled
					e.Finished = e.Scheduled.Add(time.Millisecond)
					r.Events = append(r.Events, e)
					counts[kind]++
				}
			}
			r.Events = append(r.Events, studyEvents(r.Events, p.Instances)...)
			r.Cloud = cloudSnapshot{DICOMReceipts: counts["dicom"], HL7Accepted: counts["hl7"], ReportsConfirmed: counts["report"]}
			base := r.Samples[0]
			r.Samples = nil
			for i := 0; i <= 3600; i++ {
				s := base
				s.At = r.Start.Add(time.Duration(i) * time.Second)
				r.Samples = append(r.Samples, s)
			}
			r.Supporting, r.CalibrationSupporting = nil, nil
			for i := 0; i <= 3600; i++ {
				at := r.Start.Add(time.Duration(i) * time.Second)
				for _, role := range []string{"cloud", "ris", "traffic"} {
					r.Supporting = append(r.Supporting, supportSample{At: at, Role: role})
				}
				if i <= 30 {
					for _, role := range []string{"cloud", "ris", "calibration"} {
						r.CalibrationSupporting = append(r.CalibrationSupporting, supportSample{At: at, Role: role})
					}
				}
			}
			faults := append(append([]string(nil), reportFaultCases...), dicomFaultCases...)
			for i := 0; i <= len(faults); i++ {
				base.At = r.Start.Add(time.Hour + time.Duration(i)*time.Second)
				r.FaultSamples = append(r.FaultSamples, base)
				for _, role := range []string{"cloud", "ris", "faults"} {
					r.FaultSupporting = append(r.FaultSupporting, supportSample{At: base.At, Role: role})
				}
				if i < len(faults) {
					r.Faults = append(r.Faults, faultResult{Name: faults[i], Started: base.At, Passed: true, ElapsedMS: 1000})
				}
			}
			tc.mutate(&r)
			evaluate(&r, 1, 256<<20)
			if r.Status != tc.want {
				t.Fatalf("status=%s reasons=%v", r.Status, r.Reasons)
			}
		})
	}
}

func TestScreenSeparatesQueueFromTransferAcceptance(t *testing.T) {
	for _, tt := range []struct {
		name, mode, want string
		queue, transfer  time.Duration
	}{
		{"queued-fast-transfer", "screen", "PASS", 4 * time.Second, time.Millisecond},
		{"slow-transfer", "screen", "FAIL", 0, 3 * time.Second},
		{"end-to-end-screening-retained-in-sweep", "sweep", "FAIL", 4 * time.Second, time.Millisecond},
		{"recovery-not-drained", "screen", "FAIL", 12 * time.Second, time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := passingResult(t)
			r.Mode = tt.mode
			r.Profile.Recovery = 10
			r.Events[0].Started = r.Events[0].Scheduled.Add(tt.queue)
			r.Events[0].Finished = r.Events[0].Started.Add(tt.transfer)
			evaluate(&r, 1, 256<<20)
			if r.Status != tt.want {
				t.Fatalf("%s: %v", r.Status, r.Reasons)
			}
		})
	}
}
