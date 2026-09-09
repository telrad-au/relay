package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

func testCloud(t *testing.T) (*cloudServer, []byte) {
	t.Helper()
	p, err := loadProfile("clinic-v1")
	if err != nil {
		t.Fatal(err)
	}
	p.Receipt = 1
	data := synthetic.DICOM("1.2.3.4", "1.2.3.5", 16, 16)
	r := bufio.NewReader(bytes.NewReader(data))
	m, err := synthetic.Part10(r)
	if err != nil {
		t.Fatal(err)
	}
	dataset, _ := io.ReadAll(r)
	return &cloudServer{cfg: workerConfig{Profile: p, Fixtures: []fixture{{Instance: m.Instance, Syntax: m.Syntax, DatasetBytes: int64(len(dataset)), Digest: hash(dataset)}}}}, data
}

func TestCloudRequiresByteExactCompleteDICOMBeforeReceipt(t *testing.T) {
	for _, mode := range []string{"valid", "mutated", "truncated", "unexpected-idempotency"} {
		t.Run(mode, func(t *testing.T) {
			s, data := testCloud(t)
			if mode == "mutated" {
				data[len(data)-1] ^= 1
			}
			if mode == "truncated" {
				data = data[:len(data)-3]
			}
			r := httptest.NewRequest("POST", "/v1/relay/ingest/dicom", bytes.NewReader(data))
			r.Header.Set("Content-Type", "application/dicom")
			if mode == "unexpected-idempotency" {
				r.Header.Set("Idempotency-Key", "unexpected")
			}
			w := httptest.NewRecorder()
			s.ingestDICOM(w, r)
			if mode == "valid" {
				if w.Code != 201 || s.snapshot.DICOMReceipts != 1 {
					t.Fatalf("valid upload rejected: %d", w.Code)
				}
			} else if s.snapshot.DICOMReceipts != 0 {
				t.Fatal("invalid upload received success")
			}
			if s.snapshot.ActiveUploads != 0 {
				t.Fatal("cloud upload accounting leaked")
			}
		})
	}
}

func TestCloudReportRequiresRISApplicationAAAndIdenticalResultReplay(t *testing.T) {
	s, _ := testCloud(t)
	payload := synthetic.HL7("perf-report-00000000", 4096)
	job := &reportJob{Token: "claim", Expires: time.Now().Add(time.Minute), Payload: payload, Event: event{Kind: "report"}}
	s.jobs = []*reportJob{job}
	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.control(w, httptest.NewRequest("POST", "/sessions/s/report-deliveries/delivery-0/result", strings.NewReader(body)))
		return w
	}
	accepted := `{"token":"claim","outcome":"accepted","ackCode":"AA"}`
	if call(accepted).Code == 200 {
		t.Fatal("accepted result without RIS application AA")
	}
	job.RISSends, job.RISCode = 1, "AA"
	if call(accepted).Code != 200 || !job.Confirmed {
		t.Fatal("valid result not committed")
	}
	if !job.Event.Finished.IsZero() {
		t.Fatal("report completed before Relay resolved its result response")
	}
	if call(accepted).Code != 200 || s.snapshot.ReportsConfirmed != 1 {
		t.Fatal("identical result retry was not idempotent")
	}
	s.control(httptest.NewRecorder(), httptest.NewRequest("POST", "/sessions/s/poll", strings.NewReader("{}")))
	if job.Event.Finished.IsZero() {
		t.Fatal("subsequent serial poll did not confirm report completion")
	}
	if call(`{"token":"claim","outcome":"failed","error":"changed"}`).Code == 200 {
		t.Fatal("changed result replay accepted")
	}
}

func TestCloudExpiredClaimPreservesReportBytes(t *testing.T) {
	s, _ := testCloud(t)
	payload := synthetic.HL7("perf-report-00000000", 4096)
	s.jobs = []*reportJob{{Payload: payload, Event: event{Kind: "report"}}}
	poll := func() map[string]any {
		w := httptest.NewRecorder()
		s.control(w, httptest.NewRequest("POST", "/sessions/s/poll", strings.NewReader("{}")))
		var v map[string]any
		if json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal("invalid claim")
		}
		return v
	}
	first := poll()
	s.jobs[0].Expires = time.Now().Add(-time.Second)
	second := poll()
	if first["token"] == second["token"] || first["payload"] != second["payload"] || first["messageControlId"] != second["messageControlId"] {
		t.Fatal("claim recovery changed payload or reused token")
	}
}

func TestLostResultCommitsBeforeConnectionFailure(t *testing.T) {
	s, _ := testCloud(t)
	s.behavior.Report = "lost-result"
	s.jobs = []*reportJob{{Token: "claim", Expires: time.Now().Add(time.Minute), RISSends: 1, RISCode: "AA", Event: event{Kind: "report"}}}
	server := httptest.NewServer(http.HandlerFunc(s.control))
	defer server.Close()
	body := `{"token":"claim","outcome":"accepted","ackCode":"AA"}`
	client := server.Client()
	client.Timeout = time.Second
	resp, err := client.Post(server.URL+"/delivery-0/result", "application/json", strings.NewReader(body))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("lost response was returned")
	}
	resp, err = client.Post(server.URL+"/delivery-0/result", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || s.snapshot.ReportsConfirmed != 1 {
		t.Fatal("lost committed response was not replayed")
	}
}

func TestWaitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitUntil(ctx, time.Now().Add(time.Hour)) {
		t.Fatal("cancelled timer fired")
	}
}

func TestCloudDoesNotAcknowledgeAnIncompleteUpload(t *testing.T) {
	s, data := testCloud(t)
	reader, writer := io.Pipe()
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		defer close(done)
		request := httptest.NewRequest("POST", "/v1/relay/ingest/dicom", reader)
		request.Header.Set("Content-Type", "application/dicom")
		s.ingestDICOM(w, request)
	}()
	if _, err := writer.Write(data[:len(data)-1]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("cloud responded before upload completion")
	default:
	}
	if _, err := writer.Write(data[len(data)-1:]); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("complete upload did not receive a receipt")
	}
	if w.Code != 201 {
		t.Fatalf("completed upload status=%d", w.Code)
	}
}

func TestCalibrationSeparatesCapacityFromInjectedBackpressure(t *testing.T) {
	for _, calibration := range []bool{true, false} {
		s, data := testCloud(t)
		s.cfg.Calibration = calibration
		s.cfg.Profile.BodyRate = 1 // The measured transfer cannot finish in this context.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		r := httptest.NewRequest("POST", "/v1/relay/ingest/dicom", bytes.NewReader(data)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/dicom")
		w := httptest.NewRecorder()
		s.ingestDICOM(w, r)
		cancel()
		if calibration && s.snapshot.DICOMReceipts != 1 {
			t.Fatal("intentional throttle prevented supporting calibration")
		}
		if !calibration && s.snapshot.DICOMReceipts != 0 {
			t.Fatal("measured upload ignored its configured backpressure")
		}
	}
}
