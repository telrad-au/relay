package main

import (
	"bufio"
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

func TestMixedSchedulePreservesStudyAndSizeDistributions(t *testing.T) {
	p, err := loadProfile("clinic-mixed-v1")
	if err != nil {
		t.Fatal(err)
	}
	p.Nominal = 3000
	p.Headroom = 0
	start := time.Now()
	studies := planned(p, start, "study", false)
	counts := map[string]int{}
	sizes := map[string]int{}
	for _, s := range studies {
		counts[s.Modality]++
		sizes[s.Modality+"/"+s.Size]++
	}
	for _, m := range p.Mix {
		if counts[m.Modality] != m.Weight {
			t.Fatalf("study mix: %v", counts)
		}
		for _, v := range m.Variants {
			if sizes[m.Modality+"/"+v.Name] != m.Weight*v.Weight/100 {
				t.Fatalf("size mix: %v", sizes)
			}
		}
	}
	arrivals := planned(p, start, "dicom", false)
	templates := p.studyTemplates()
	total := 0
	for _, s := range studies {
		total += s.Instances
	}
	if len(arrivals) != total {
		t.Fatal("partial study scheduled")
	}
	for _, e := range arrivals {
		s := studies[e.Study]
		template := templates[e.Template]
		if e.Scheduled != s.Scheduled || e.Fixture < template.First || e.Fixture >= template.First+template.Count {
			t.Fatal("study burst or fixture identity lost")
		}
	}
	if templates[8].Bytes != 1200<<20 || templates[8].Count != 2400 {
		t.Fatal("large CT is no longer 2400 images / 1200 MiB")
	}
	// Dropping the final object must not turn a partial study into success.
	for i := range arrivals {
		arrivals[i].Started = arrivals[i].Scheduled
		arrivals[i].Finished = arrivals[i].Scheduled.Add(time.Millisecond)
	}
	actual := mixedStudyEvents(arrivals[:len(arrivals)-1], p, start)
	if actual[len(actual)-1].Error != "incomplete_study" {
		t.Fatal("missing final instance accepted")
	}
}

func TestMixedCalibrationPreservesTheOfferedFixtureMix(t *testing.T) {
	p, err := loadProfile("clinic-mixed-v1")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	nominal := planned(p, start, "dicom", false)
	calibration := planned(p, start, "dicom", true)
	if len(nominal) != len(calibration) {
		t.Fatal("calibration omitted mixed studies")
	}
	for i, e := range calibration {
		if e.Fixture != nominal[i].Fixture || e.Study != nominal[i].Study {
			t.Fatal("calibration fixture mix differs")
		}
		want := nominal[i].Scheduled.Sub(start.Add(seconds(p.Idle))).Seconds() / (2 * p.Factor)
		if delta := e.Scheduled.Sub(start).Seconds() - want; delta > 1e-7 || delta < -1e-7 {
			t.Fatal("calibration offered rate changed")
		}
	}
}

func TestMixedVerdictRejectsMissingAndRelabelledWork(t *testing.T) {
	for _, name := range []string{"valid", "queue-only", "missing", "relabelled", "deadline", "calibration-missing", "calibration-early"} {
		t.Run(name, func(t *testing.T) {
			r := passingResult(t)
			p, err := loadProfile("clinic-mixed-v1")
			if err != nil {
				t.Fatal(err)
			}
			r.Profile = p
			r.Mode = "screen"
			r.Events = nil
			r.Calibration = nil
			for _, kind := range []string{"dicom", "hl7", "report"} {
				for _, e := range planned(p, r.Start, kind, false) {
					e.Started = e.Scheduled
					e.Finished = e.Scheduled.Add(time.Millisecond)
					r.Events = append(r.Events, e)
				}
				for _, e := range planned(p, r.Start, kind, true) {
					e.Started = e.Scheduled
					e.Finished = e.Scheduled.Add(time.Millisecond)
					r.Calibration = append(r.Calibration, e)
				}
			}
			r.Events = append(r.Events, mixedStudyEvents(r.Events, p, r.Start)...)
			s := summarize(r.Events, 1)
			r.Cloud = cloudSnapshot{DICOMReceipts: s["dicom"].Completed, HL7Accepted: s["hl7"].Completed, ReportsConfirmed: s["report"].Completed}
			switch name {
			case "queue-only":
				for i := range r.Events {
					if (r.Events[i].Kind == "dicom" || r.Events[i].Kind == "study") && r.Events[i].Study == 0 {
						r.Events[i].Started = r.Events[i].Scheduled.Add(millis(r.Events[i].LimitMS + 1))
						r.Events[i].Finished = r.Events[i].Started.Add(time.Millisecond)
					}
				}
			case "missing":
				r.Events = r.Events[1:]
			case "relabelled":
				r.Events[0].Modality = "CT"
			case "deadline":
				r.Events[0].Finished = r.Events[0].Scheduled.Add(millis(r.Events[0].LimitMS + 1))
			case "calibration-missing":
				r.Calibration = r.Calibration[1:]
			case "calibration-early":
				r.Calibration[0].Started = r.Calibration[0].Scheduled.Add(-time.Millisecond)
			}
			evaluate(&r, 1, 256<<20)
			wantPass := name == "valid" || name == "queue-only"
			if wantPass && r.Status != "PASS" || !wantPass && r.Status == "PASS" {
				t.Fatalf("%s %v", r.Status, r.Reasons)
			}
		})
	}
}

func TestCloudValidatesEveryModalityAndMultiFrameBytes(t *testing.T) {
	for _, m := range []string{"CT", "MR", "DX", "US"} {
		for _, frames := range []int{1, 3} {
			if m != "US" && frames > 1 {
				continue
			}
			t.Run(m+time.Duration(frames).String(), func(t *testing.T) {
				p, _ := loadProfile("clinic-v1")
				p.Receipt = 0
				spec := synthetic.ImageSpec{Modality: m, Rows: 16, Columns: 16, Bits: 16, Samples: 1, Frames: frames}
				if m == "US" {
					spec.Bits = 8
					spec.Samples = 3
				}
				h, err := synthetic.ModalityHeader(spec, "1.2.3.1", "1.2.3.2", "1.2.3.3", 1, 1)
				if err != nil {
					t.Fatal(err)
				}
				pixels, _ := synthetic.ModalityPixels(spec)
				data := append(h, pixels...)
				source := bytes.NewReader(data)
				reader := bufio.NewReader(source)
				meta, err := synthetic.Part10(reader)
				if err != nil {
					t.Fatal(err)
				}
				offset := len(data) - reader.Buffered() - source.Len()
				f := fixture{Instance: meta.Instance, Class: meta.Class, Syntax: meta.Syntax, DatasetBytes: int64(len(data) - offset), Digest: hash(data[offset:])}
				for _, corrupt := range []bool{false, true} {
					body := append([]byte(nil), data...)
					if corrupt {
						body[len(body)-1] ^= 1
					}
					server := cloudServer{cfg: workerConfig{Profile: p, Fixtures: []fixture{f}}}
					req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(context.Background())
					req.Header.Set("Content-Type", "application/dicom")
					w := httptest.NewRecorder()
					server.ingestDICOM(w, req)
					if !corrupt && w.Code != 201 || corrupt && w.Code != 400 {
						t.Fatalf("corrupt=%v status=%d", corrupt, w.Code)
					}
				}
			})
		}
	}
}
