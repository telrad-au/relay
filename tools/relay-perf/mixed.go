package main

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

type imageGroup struct {
	synthetic.ImageSpec
	Instances int `json:"instances"`
	PerSeries int `json:"instancesPerSeries"`
}
type studyVariant struct {
	Name   string       `json:"name"`
	Weight int          `json:"weight"`
	Groups []imageGroup `json:"groups"`
}
type modalityMix struct {
	Modality string         `json:"modality"`
	Weight   int            `json:"weight"`
	Variants []studyVariant `json:"variants"`
}
type studyTemplate struct {
	Modality, Size string
	First, Count   int
	Bytes          int64
}

func (p profile) validateMix() error {
	if p.DICOMRate != 0 || p.Rows != 0 || p.Columns != 0 || p.Instances != 0 || p.StudyRate <= 0 || math.IsNaN(p.StudyRate) || math.IsInf(p.StudyRate, 0) || p.TransferMargin < 1 || p.TransferMargin > 4 || math.IsNaN(p.TransferMargin) || len(p.Mix) > 8 {
		return errors.New("mixed studies require a study rate, transfer margin and no uniform-image settings")
	}
	if p.Syntax != synthetic.ExplicitVRLittleEndian {
		return errors.New("mixed fixture profile currently requires Explicit VR Little Endian")
	}
	weights := 0
	count := 0
	var bytes int64
	modalities := map[string]bool{}
	for _, m := range p.Mix {
		if m.Weight < 1 || m.Weight > 100 || modalities[m.Modality] || len(m.Variants) != 3 {
			return errors.New("invalid modality weights or variants")
		}
		modalities[m.Modality] = true
		weights += m.Weight
		vw := 0
		names := map[string]bool{}
		for _, v := range m.Variants {
			if (v.Name != "small" && v.Name != "medium" && v.Name != "large") || names[v.Name] || v.Weight < 1 || v.Weight > 100 || len(v.Groups) < 1 || len(v.Groups) > 8 {
				return errors.New("invalid study size distribution")
			}
			names[v.Name] = true
			vw += v.Weight
			for _, g := range v.Groups {
				if g.Modality != m.Modality || g.Validate() != nil || g.Instances < 1 || g.Instances > 3000 || g.PerSeries < 1 || g.PerSeries > g.Instances {
					return errors.New("invalid modality image group")
				}
				count += g.Instances
				bytes += int64(g.Instances) * g.PixelBytes()
			}
		}
		if vw != 100 {
			return errors.New("study size weights must sum to 100")
		}
	}
	if weights != 100 || count > 20000 || bytes > 8<<30 {
		return errors.New("invalid mix weights or fixture corpus exceeds 8 GiB / 20000 objects")
	}
	if p.StudyRate*(p.Nominal+p.Factor*p.Headroom)*float64(count) > 2e6 {
		return errors.New("mixed schedule exceeds bounded event capacity")
	}
	return nil
}

func (p profile) studyTemplates() []studyTemplate {
	var templates []studyTemplate
	first := 0
	for _, m := range p.Mix {
		for _, v := range m.Variants {
			t := studyTemplate{Modality: m.Modality, Size: v.Name, First: first}
			for _, g := range v.Groups {
				t.Count += g.Instances
				t.Bytes += int64(g.Instances) * g.PixelBytes()
			}
			first += t.Count
			templates = append(templates, t)
		}
	}
	return templates
}

// Smooth weighted round-robin gives exact counts each 100 studies without
// putting all CT or large cases together. Selection never depends on senders.
func weightedChoice(weights []int, sequence int) int {
	current := make([]int, len(weights))
	total := 0
	for _, w := range weights {
		total += w
	}
	selected := 0
	for step := 0; step <= sequence%total; step++ {
		selected = 0
		for i, w := range weights {
			current[i] += w
			if current[i] > current[selected] {
				selected = i
			}
		}
		current[selected] -= total
	}
	return selected
}

func (p profile) templateForStudy(sequence int) int {
	weights := make([]int, len(p.Mix))
	for i, m := range p.Mix {
		weights[i] = m.Weight
	}
	modality := weightedChoice(weights, sequence)
	occurrence := 0
	for i := 0; i < sequence; i++ {
		if weightedChoice(weights, i) == modality {
			occurrence++
		}
	}
	weights = nil
	offset := 0
	for i, m := range p.Mix {
		if i < modality {
			offset += len(m.Variants)
		}
		if i == modality {
			for _, v := range m.Variants {
				weights = append(weights, v.Weight)
			}
		}
	}
	return offset + weightedChoice(weights, occurrence)
}

func (p profile) calibrationSeconds() float64 {
	if len(p.Mix) > 0 {
		return (p.Nominal + p.Headroom) / (2 * p.Factor)
	}
	return 30
}

func (p profile) studyBudget(t studyTemplate) float64 {
	// Includes serialized cloud receipts distributed over the offered association
	// count, transfer time, and a declared margin for contention. Units: ms.
	return 5000 + p.TransferMargin*(float64(t.Bytes)*8/(p.Bandwidth*1e6)*1000+float64(t.Count)*(p.Receipt+p.RTT)/float64(p.DICOMConnections))
}

func mixedPlanned(p profile, start time.Time, kind string, calibration bool) []event {
	templates := p.studyTemplates()
	var out []event
	study := 0
	sequence := 0
	for _, phase := range []struct {
		name                     string
		offset, duration, factor float64
	}{{"nominal", 0, p.Nominal, 1}, {"headroom", p.Nominal, p.Headroom, p.Factor}} {
		for i := 0; i < int(math.Ceil(p.StudyRate*phase.factor*phase.duration)); i++ {
			index := p.templateForStudy(study)
			t := templates[index]
			offset := phase.offset + float64(i)/(p.StudyRate*phase.factor)
			phaseName := phase.name
			if calibration {
				offset /= 2 * p.Factor
				phaseName = "calibration"
			} else {
				offset += p.Idle
			}
			e := event{Kind: kind, Sequence: study, Study: study, Template: index, Modality: t.Modality, Size: t.Size, Phase: phaseName, Scheduled: start.Add(seconds(offset)), LimitMS: p.studyBudget(t), Instances: t.Count}
			if kind == "study" {
				out = append(out, e)
			} else {
				for j := 0; j < t.Count; j++ {
					e.Sequence = sequence
					e.Fixture = t.First + j
					out = append(out, e)
					sequence++
				}
			}
			study++
		}
	}
	return out
}

func mixedStudyEvents(events []event, p profile, start time.Time) []event {
	groups := map[int][]event{}
	for _, e := range events {
		if e.Kind == "dicom" {
			groups[e.Study] = append(groups[e.Study], e)
		}
	}
	out := mixedPlanned(p, start, "study", false)
	for i := range out {
		e := &out[i]
		members := groups[e.Sequence]
		seen := map[int]bool{}
		if len(members) != e.Instances {
			e.Error = "incomplete_study"
		}
		for _, m := range members {
			if seen[m.Sequence] || m.Error != "" || m.Finished.IsZero() {
				e.Error = "incomplete_study"
			}
			seen[m.Sequence] = true
			if !m.Started.IsZero() && (e.Started.IsZero() || m.Started.Before(e.Started)) {
				e.Started = m.Started
			}
			if m.Finished.After(e.Finished) {
				e.Finished = m.Finished
			}
			e.Bytes += m.Bytes
		}
	}
	return out
}

func mixedSummaries(events []event, duration float64) map[string]map[string]summary {
	groups := map[string][]event{}
	for _, e := range events {
		if e.Modality != "" {
			groups[e.Modality+"/"+e.Size] = append(groups[e.Modality+"/"+e.Size], e)
		}
	}
	out := map[string]map[string]summary{}
	for name, es := range groups {
		out[name] = summarize(es, duration)
	}
	return out
}

func sortEvents(events []event) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Kind != events[j].Kind {
			return events[i].Kind < events[j].Kind
		}
		return events[i].Sequence < events[j].Sequence
	})
}

func mixedCalibrationCheck(events []event, p profile, latencyGate bool) error {
	var start time.Time
	for _, e := range events {
		if start.IsZero() || e.Scheduled.Before(start) {
			start = e.Scheduled
		}
	}
	for _, kind := range []string{"dicom", "hl7", "report"} {
		want := planned(p, start, kind, true)
		seen := map[int]bool{}
		for _, e := range events {
			if e.Kind != kind {
				continue
			}
			if e.Sequence < 0 || e.Sequence >= len(want) || seen[e.Sequence] || e.Started.IsZero() || e.Finished.IsZero() || e.Finished.Before(e.Started) || e.Started.Before(e.Scheduled) || e.Error != "" || !e.Scheduled.Equal(want[e.Sequence].Scheduled) {
				return errors.New("support calibration lost or changed offered work")
			}
			seen[e.Sequence] = true
			limit := map[string]float64{"hl7": p.HL7P99, "report": p.ReportP99}[kind]
			if kind == "dicom" {
				limit = want[e.Sequence].LimitMS
				if e.Fixture != want[e.Sequence].Fixture || e.Study != want[e.Sequence].Study {
					return errors.New("support calibration changed fixture mix")
				}
			}
			if latencyGate && e.Finished.Sub(e.Scheduled) > millis(limit) {
				return errors.New("support calibration exceeded declared deadline")
			}
		}
		if len(seen) != len(want) {
			return errors.New("support calibration incomplete")
		}
	}
	return nil
}
