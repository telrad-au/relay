package main

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Events contain synthetic sequence numbers, never payloads, UIDs, control IDs,
// authorization headers, credentials, or claim tokens.
type event struct {
	Study     int       `json:"study,omitempty"`
	Template  int       `json:"template,omitempty"`
	Fixture   int       `json:"fixture,omitempty"`
	Modality  string    `json:"modality,omitempty"`
	Size      string    `json:"size,omitempty"`
	LimitMS   float64   `json:"limitMillis,omitempty"`
	Instances int       `json:"instances,omitempty"`
	Kind      string    `json:"kind"`
	Sequence  int       `json:"sequence"`
	Phase     string    `json:"phase"`
	Scheduled time.Time `json:"scheduled"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Bytes     int64     `json:"bytes"`
	Error     string    `json:"error,omitempty"`
	UploadMS  float64   `json:"uploadMillis,omitempty"`
	ReceiptMS float64   `json:"receiptMillis,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
}

type sample struct {
	At            time.Time `json:"at"`
	RSS           uint64    `json:"rssBytes"`
	PeakRSS       uint64    `json:"peakRssBytes"`
	Memory        uint64    `json:"cgroupBytes"`
	Peak          uint64    `json:"cgroupPeakBytes"`
	Limit         uint64    `json:"memoryLimitBytes"`
	SwapLimit     uint64    `json:"swapLimitBytes"`
	CPUQuota      float64   `json:"cpuQuota"`
	CPUUse        uint64    `json:"cpuUseMicros"`
	Throttled     uint64    `json:"throttledMicros"`
	OOM           uint64    `json:"oomKills"`
	HostIdle      float64   `json:"hostIdlePercent"`
	HostAvailable uint64    `json:"hostAvailableBytes"`
	HostTotal     uint64    `json:"hostTotalBytes"`
	ReadBytes     uint64    `json:"readBytes"`
	WriteBytes    uint64    `json:"writeBytes"`
	Error         string    `json:"error,omitempty"`
}

type latencyDistribution struct {
	P50 float64 `json:"p50Millis"`
	P95 float64 `json:"p95Millis"`
	P99 float64 `json:"p99Millis"`
	Max float64 `json:"maxMillis"`
}

func distribution(values []float64) latencyDistribution {
	return latencyDistribution{percentile(values, .5), percentile(values, .95), percentile(values, .99), percentile(values, 1)}
}

type summary struct {
	Queue       latencyDistribution `json:"senderQueue"`
	Transfer    latencyDistribution `json:"transfer"`
	Count       int                 `json:"offered"`
	Started     int                 `json:"started"`
	Completed   int                 `json:"completed"`
	Failed      int                 `json:"failed"`
	Outstanding int                 `json:"outstanding"`
	Bytes       int64               `json:"completedBytes"`
	P50         float64             `json:"p50Millis"`
	P95         float64             `json:"p95Millis"`
	P99         float64             `json:"p99Millis"`
	Max         float64             `json:"maxMillis"`
	MaxBacklog  int                 `json:"maxBacklog"`
	PerSecond   float64             `json:"completedPerSecond"`
}

type result struct {
	LatencyBasis          string                        `json:"latencyAcceptanceBasis"`
	Modalities            map[string]map[string]summary `json:"modalities,omitempty"`
	Schema                int                           `json:"schemaVersion"`
	Mode                  string                        `json:"mode"`
	Status                string                        `json:"status"`
	Reasons               []string                      `json:"reasons"`
	Profile               profile                       `json:"profile"`
	Start                 time.Time                     `json:"start"`
	Revision              string                        `json:"revision"`
	Dirty                 bool                          `json:"dirty"`
	Image                 string                        `json:"imageId"`
	Toolchain             string                        `json:"goVersion"`
	Host                  map[string]any                `json:"host"`
	Fixtures              []fixture                     `json:"fixtures"`
	Summary               map[string]summary            `json:"summary"`
	Samples               []sample                      `json:"relaySamples"`
	FaultSupporting       []supportSample               `json:"faultSupportingSamples"`
	FaultSamples          []sample                      `json:"faultSamples"`
	Supporting            []supportSample               `json:"supportingSamples"`
	CalibrationSupporting []supportSample               `json:"calibrationSupportingSamples"`
	Calibration           []event                       `json:"calibration"`
	Events                []event                       `json:"events"`
	Cloud                 cloudSnapshot                 `json:"cloud"`
	Faults                []faultResult                 `json:"faults"`
	CleanupErrors         []string                      `json:"cleanupErrors"`
}

type eventLog struct {
	sync.Mutex
	Events []event
}

func (l *eventLog) add(e event) { l.Lock(); defer l.Unlock(); l.Events = append(l.Events, e) }
func (l *eventLog) snapshot() []event {
	l.Lock()
	defer l.Unlock()
	return append([]event(nil), l.Events...)
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	return v[max(0, int(math.Ceil(float64(len(v))*p))-1)]
}

func summarize(events []event, duration float64) map[string]summary {
	out := map[string]summary{}
	for _, kind := range []string{"dicom", "hl7", "report", "study"} {
		var s summary
		var latencies, queues, transfers []float64
		type change struct {
			at    time.Time
			delta int
		}
		var changes []change
		for _, e := range events {
			if e.Kind != kind {
				continue
			}
			s.Count++
			changes = append(changes, change{e.Scheduled, 1})
			if !e.Started.IsZero() {
				s.Started++
				queues = append(queues, float64(e.Started.Sub(e.Scheduled))/float64(time.Millisecond))
			}
			if e.Finished.IsZero() {
				s.Outstanding++
				continue
			}
			changes = append(changes, change{e.Finished, -1})
			if e.Error != "" {
				s.Failed++
				continue
			}
			s.Completed++
			if !e.Started.IsZero() {
				transfers = append(transfers, float64(e.Finished.Sub(e.Started))/float64(time.Millisecond))
			}
			s.Bytes += e.Bytes
			latencies = append(latencies, float64(e.Finished.Sub(e.Scheduled))/float64(time.Millisecond))
		}
		sort.Slice(changes, func(i, j int) bool {
			if changes[i].at.Equal(changes[j].at) {
				return changes[i].delta < changes[j].delta
			}
			return changes[i].at.Before(changes[j].at)
		})
		backlog := 0
		for _, c := range changes {
			backlog += c.delta
			s.MaxBacklog = max(s.MaxBacklog, backlog)
		}
		s.Queue, s.Transfer = distribution(queues), distribution(transfers)
		s.P50, s.P95, s.P99, s.Max = percentile(latencies, .5), percentile(latencies, .95), percentile(latencies, .99), percentile(latencies, 1)
		if duration > 0 {
			s.PerSecond = float64(s.Completed) / duration
		}
		out[kind] = s
	}
	return out
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}

func evaluate(r *result, cpus float64, memory int64) {
	r.Status = "PASS"
	r.LatencyBasis = "scheduled-to-finished"
	if r.Mode == "screen" {
		r.LatencyBasis = "started-to-finished"
	}
	uncertain := false
	defer func() {
		if uncertain {
			r.Status = "INCONCLUSIVE"
		}
	}()
	addReason := func(reason string) {
		if !slices.Contains(r.Reasons, reason) {
			r.Reasons = append(r.Reasons, reason)
		}
	}
	fail := func(reason string) { addReason(reason); r.Status = "FAIL" }
	inconclusive := func(reason string) {
		uncertain = true
		addReason(reason)
	}
	p := r.Profile
	r.Summary = summarize(r.Events, p.Nominal+p.Headroom)
	if len(p.Mix) > 0 {
		r.Modalities = mixedSummaries(r.Events, p.Nominal+p.Headroom)
	}
	if len(r.CleanupErrors) > 0 {
		fail("cleanup_failed")
	}
	for _, kind := range []string{"dicom", "hl7", "report", "study"} {
		s := r.Summary[kind]
		arrivals := planned(p, r.Start, kind, false)
		expected := len(arrivals)
		if kind == "study" && len(p.Mix) == 0 {
			expected = (len(planned(p, r.Start, "dicom", false)) + p.Instances - 1) / p.Instances
		}
		if s.Count != expected {
			fail(kind + "_offered_count_mismatch")
		}
		seen := map[int]bool{}
		for _, e := range r.Events {
			if e.Kind != kind {
				continue
			}
			if seen[e.Sequence] || e.Sequence < 0 || e.Sequence >= expected || ((kind != "study" || len(p.Mix) > 0) && !e.Scheduled.Equal(arrivals[e.Sequence].Scheduled)) {
				fail(kind + "_offered_identity_mismatch")
				break
			}
			seen[e.Sequence] = true
			if len(p.Mix) > 0 && (kind == "dicom" || kind == "study") {
				want := arrivals[e.Sequence]
				if e.Study != want.Study || e.Fixture != want.Fixture || e.Template != want.Template || e.Modality != want.Modality || e.Size != want.Size || e.Instances != want.Instances || e.LimitMS != want.LimitMS {
					fail(kind + "_offered_identity_mismatch")
				}
				baseline := e.Scheduled
				if r.Mode == "screen" {
					baseline = e.Started
				}
				if r.Mode != "smoke" && !baseline.IsZero() && e.Finished.Sub(baseline) > millis(want.LimitMS) {
					fail(kind + "_size_deadline")
				}
			}
			if (!e.Started.IsZero() && e.Started.Before(e.Scheduled)) || (!e.Finished.IsZero() && (e.Started.IsZero() || e.Finished.Before(e.Scheduled) || e.Finished.Before(e.Started))) {
				fail(kind + "_invalid_timestamps")
			}
			if r.Mode != "smoke" && e.Finished.After(r.Start.Add(p.total())) {
				fail(kind + "_recovery_not_drained")
			}
			if r.Mode != "smoke" && r.Mode != "screen" && kind != "study" && !e.Started.IsZero() {
				limit := map[string]float64{"dicom": p.DICOMP99, "hl7": p.HL7P99, "report": p.ReportP99}[kind]
				if kind == "dicom" && len(p.Mix) > 0 {
					limit = arrivals[e.Sequence].LimitMS
				}
				if e.Started.Sub(e.Scheduled) > millis(limit) {
					fail(kind + "_queue_wait")
				}
			}
		}
		if s.Count == 0 || s.Completed != s.Count || s.Failed > 0 || s.Outstanding > 0 {
			fail(kind + "_incomplete")
		}
		if r.Mode != "smoke" && !(len(p.Mix) > 0 && (kind == "dicom" || kind == "study")) {
			limit := map[string]float64{"dicom": p.DICOMP99, "hl7": p.HL7P99, "report": p.ReportP99, "study": p.StudyMax}[kind]
			p99, maximum := s.P99, s.Max
			if r.Mode == "screen" {
				p99, maximum = s.Transfer.P99, s.Transfer.Max
			}
			if p99 > limit || kind == "study" && maximum > limit {
				fail(kind + "_latency")
			}
		}
	}
	if r.Cloud.Errors > 0 || r.Cloud.ActiveUploads != 0 || r.Cloud.DICOMReceipts != r.Summary["dicom"].Completed || r.Cloud.HL7Accepted != r.Summary["hl7"].Completed || r.Cloud.ReportsConfirmed != r.Summary["report"].Completed {
		fail("cloud_accounting_or_integrity")
	}
	if len(r.Samples) < 2 {
		inconclusive("missing_resource_samples")
	}
	var first, last []float64
	hostPressure := 0
	for _, s := range append(append([]sample(nil), r.Samples...), r.FaultSamples...) {
		if s.Error != "" {
			inconclusive("resource_sample_failed")
			break
		}
		if s.Limit != uint64(memory) || s.SwapLimit != 0 || math.Abs(s.CPUQuota-cpus) > .001 {
			inconclusive("effective_resource_limits_mismatch")
			break
		}
		if s.OOM > 0 {
			fail("relay_oom")
		}
		if s.HostTotal == 0 || s.HostAvailable == 0 {
			inconclusive("host_memory_measurements_unavailable")
			break
		}
		if s.HostIdle < 10 || float64(s.HostAvailable) < float64(s.HostTotal)*.1 {
			hostPressure++
		} else {
			hostPressure = 0
		}
		if hostPressure >= 10 {
			inconclusive("host_capacity_exhausted")
			break
		}
		if r.Mode != "smoke" && s.Peak >= uint64(float64(memory)*.8) {
			fail("memory_headroom")
		}
		elapsed := s.At.Sub(r.Start).Seconds()
		if elapsed >= p.Idle && elapsed < p.Idle+600 {
			first = append(first, float64(s.RSS))
		}
		if elapsed >= p.Idle+p.Nominal-600 && elapsed < p.Idle+p.Nominal {
			last = append(last, float64(s.RSS))
		}
	}
	if r.Mode == "qualify" {
		var window []sample
		end := r.Start.Add(p.total())
		for _, s := range r.Samples {
			if !s.At.Before(r.Start) && !s.At.After(end) {
				window = append(window, s)
			}
		}
		if len(window) < 2 || window[0].At.Sub(r.Start) > 5*time.Second || end.Sub(window[len(window)-1].At) > 5*time.Second {
			inconclusive("resource_observation_incomplete")
		}
		for i := 1; i < len(window); i++ {
			if window[i].At.Sub(window[i-1].At) > 5*time.Second || !window[i].At.After(window[i-1].At) {
				inconclusive("resource_observation_gap")
				break
			}
		}
		if p.Nominal < 1200 || len(first) < 500 || len(last) < 500 {
			inconclusive("insufficient_memory_stability_window")
		} else {
			a, b := percentile(first, .95), percentile(last, .95)
			if b-a > math.Max(8*1024*1024, a*.1) {
				fail("resident_memory_growth")
			}
		}
	}
	if len(r.Supporting) < 2 || len(r.CalibrationSupporting) < 2 {
		inconclusive("missing_supporting_samples")
	}
	for _, phase := range []struct {
		samples   []supportSample
		generator string
	}{{r.Supporting, "traffic"}, {r.CalibrationSupporting, "calibration"}} {
		roles := []string{"cloud", "ris", phase.generator}
		if r.Host["relayDocker"] != nil {
			roles = append(roles, "support-host", "proxy")
		}
		if strings.HasPrefix(r.Mode, "external") {
			roles = append(roles, "proxy")
		}
		for _, role := range roles {
			count := 0
			var times []time.Time
			for _, s := range phase.samples {
				if s.Role == role || strings.HasPrefix(s.Role, role+"-") {
					count++
					times = append(times, s.At)
				}
			}
			if count < 2 {
				inconclusive("missing_" + role + "_measurements")
			}
			if r.Mode == "qualify" {
				start, end := r.Start, r.Start.Add(p.total())
				if phase.generator == "calibration" {
					start = time.Time{}
					for _, e := range r.Calibration {
						if start.IsZero() || e.Scheduled.Before(start) {
							start = e.Scheduled
						}
					}
					end = start.Add(seconds(p.calibrationSeconds()))
				}
				if !observedThroughout(times, start, end, 10*time.Second) {
					inconclusive(role + "_observation_gap")
				}
			}
		}
	}
	for _, s := range append(append([]supportSample(nil), r.Supporting...), append(r.CalibrationSupporting, r.FaultSupporting...)...) {
		if s.Error != "" || s.Saturated {
			inconclusive("supporting_system_capacity")
			break
		}
	}
	if err := calibrationCheck(r.Calibration, p, r.Mode != "smoke"); err != nil {
		inconclusive("supporting_calibration_failed")
	}
	for _, f := range r.Faults {
		if !f.Passed {
			fail("fault_" + f.Name)
		}
	}
	if r.Mode == "qualify" {
		var faultStart, faultEnd time.Time
		for _, f := range r.Faults {
			if faultStart.IsZero() || f.Started.Before(faultStart) {
				faultStart = f.Started
			}
			if end := f.Started.Add(millis(f.ElapsedMS)); end.After(faultEnd) {
				faultEnd = end
			}
		}
		var times []time.Time
		for _, s := range r.FaultSamples {
			times = append(times, s.At)
		}
		if !observedThroughout(times, faultStart, faultEnd, 5*time.Second) {
			inconclusive("fault_resource_observation_gap")
		}
		for _, role := range []string{"cloud", "ris", "faults"} {
			times = nil
			for _, s := range r.FaultSupporting {
				if s.Role == role || strings.HasPrefix(s.Role, role+"-") {
					times = append(times, s.At)
				}
			}
			if !observedThroughout(times, faultStart, faultEnd, 10*time.Second) {
				inconclusive("fault_" + role + "_observation_gap")
			}
		}
		if len(r.FaultSupporting) < 2 {
			inconclusive("missing_fault_supporting_samples")
		}
		if len(r.FaultSamples) < 2 {
			inconclusive("missing_fault_resource_samples")
		}
		for _, name := range append(append([]string(nil), reportFaultCases...), dicomFaultCases...) {
			count := 0
			for _, f := range r.Faults {
				if f.Name == name {
					count++
				}
			}
			if count != 1 {
				inconclusive("missing_or_duplicate_fault_" + name)
			}
		}
	}
}

// Missing intervals cannot establish that supporting capacity or fault recovery
// stayed observable. Ignore setup/cleanup samples outside the measured interval.
func observedThroughout(times []time.Time, start, end time.Time, gap time.Duration) bool {
	if start.IsZero() || !end.After(start) {
		return false
	}
	last := start
	count := 0
	for _, at := range times {
		if at.Before(start) || at.After(end) {
			continue
		}
		if count > 0 && !at.After(last) || at.Sub(last) > gap {
			return false
		}
		last = at
		count++
	}
	return count >= 2 && end.Sub(last) <= gap
}

func calibrationOK(events []event, p profile) error {
	return calibrationCheck(events, p, true)
}

func calibrationCheck(events []event, p profile, latencyGate bool) error {
	if len(p.Mix) > 0 {
		return mixedCalibrationCheck(events, p, latencyGate)
	}
	s := summarize(events, 30)
	for _, kind := range []string{"dicom", "hl7", "report"} {
		want := int(math.Ceil(map[string]float64{"dicom": p.DICOMRate, "hl7": p.HL7Rate, "report": p.ReportRate}[kind] * p.Factor * 2 * 30))
		if s[kind].Count != want || s[kind].Completed != want || latencyGate && s[kind].P99 > map[string]float64{"dicom": p.DICOMP99, "hl7": p.HL7P99, "report": p.ReportP99}[kind] {
			return errors.New("support calibration did not sustain twice headroom traffic")
		}
	}
	return nil
}
