package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

type workerConfig struct {
	FixtureDirectory string    `json:"fixtureDirectory,omitempty"`
	Profile          profile   `json:"profile"`
	Origin           string    `json:"origin"`
	Admin            string    `json:"admin"`
	Relay            string    `json:"relay"`
	RIS              string    `json:"ris"`
	Credential       string    `json:"credential"`
	Fixtures         []fixture `json:"fixtures"`
	Mode             string    `json:"mode"`
	Calibration      bool      `json:"calibration"`
}

type cloudSnapshot struct {
	DICOMReceipts    int     `json:"dicomReceipts"`
	HL7Accepted      int     `json:"hl7Accepted"`
	ReportsConfirmed int     `json:"reportsConfirmed"`
	Polls            int     `json:"polls"`
	ResultRequests   int     `json:"resultRequests"`
	Errors           int     `json:"errors"`
	ActiveUploads    int     `json:"activeUploads"`
	Events           []event `json:"events"`
}

type reportJob struct {
	Event     event
	Token     string
	Expires   time.Time
	Claim     int
	Payload   []byte
	Confirmed bool
	Result    []byte
	RISSends  int
	RISCode   string
}

type behavior struct {
	Report        string  `json:"report"`
	DICOM         string  `json:"dicom"`
	ReceiptMillis float64 `json:"receiptMillis"`
	ACKMillis     float64 `json:"ackMillis"`
}

type cloudServer struct {
	sync.Mutex
	cfg      workerConfig
	snapshot cloudSnapshot
	jobs     []*reportJob
	behavior behavior
	dicom    []event
}

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func decodeRequest(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(v)
}
func hash(b []byte) string                  { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }
func controlID(kind string, seq int) string { return fmt.Sprintf("perf-%s-%08d", kind, seq) }
func parseControlID(b []byte, kind string) (int, error) {
	line := strings.SplitN(string(b), "\r", 2)[0]
	fields := strings.Split(line, "|")
	if len(fields) < 10 || !strings.HasPrefix(fields[9], "perf-"+kind+"-") {
		return 0, errors.New("unexpected synthetic control ID")
	}
	i, err := strconv.Atoi(strings.TrimPrefix(fields[9], "perf-"+kind+"-"))
	if i < 0 {
		return 0, errors.New("invalid synthetic sequence")
	}
	return i, err
}

func (s *cloudServer) bad(w http.ResponseWriter) {
	s.Lock()
	s.snapshot.Errors++
	s.Unlock()
	http.Error(w, "request rejected", 400)
}

func (s *cloudServer) ingestDICOM(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/dicom" || r.Header.Get("Idempotency-Key") != "" {
		s.bad(w)
		return
	}
	s.Lock()
	s.snapshot.ActiveUploads++
	b := s.behavior
	calibrating := s.cfg.Calibration
	s.Unlock()
	defer func() { s.Lock(); s.snapshot.ActiveUploads--; s.Unlock() }()
	start := time.Now()
	reader := bufio.NewReaderSize(r.Body, 32*1024)
	meta, err := synthetic.Part10(reader)
	if err != nil {
		http.Error(w, "incomplete Part 10 header", 400)
		return
	} // Interrupted streams are accounted by the fault driver.
	var expected *fixture
	for i := range s.cfg.Fixtures {
		if s.cfg.Fixtures[i].Instance == meta.Instance {
			expected = &s.cfg.Fixtures[i]
			break
		}
	}
	if expected == nil || meta.Class != expected.storageClass() || meta.Syntax != expected.Syntax {
		s.bad(w)
		return
	}
	if b.DICOM == "stall" {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Second):
		}
	}
	digest := sha256.New()
	var count int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			count += int64(n)
			_, _ = digest.Write(buf[:n])
			if count > expected.DatasetBytes {
				s.bad(w)
				return
			}
			rate := s.cfg.Profile.BodyRate
			// Establish supporting capacity before imposing intentional upload
			// backpressure on the measured Relay workload.
			if calibrating {
				rate = 0
			}
			if b.DICOM == "slow-progress" {
				rate = 64 * 1024
			}
			if rate > 0 && !waitUntil(r.Context(), start.Add(time.Duration(float64(count)/float64(rate)*float64(time.Second)))) {
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return
		}
	}
	if count != expected.DatasetBytes || hex.EncodeToString(digest.Sum(nil)) != expected.Digest {
		s.bad(w)
		return
	}
	upload := time.Now()
	delay := s.cfg.Profile.Receipt
	if b.ReceiptMillis > 0 {
		delay = b.ReceiptMillis
	}
	if !waitUntil(r.Context(), upload.Add(millis(delay))) {
		return
	}
	if b.DICOM == "reject" {
		http.Error(w, "synthetic fault", 503)
		return
	}
	s.Lock()
	s.snapshot.DICOMReceipts++
	seq := s.snapshot.DICOMReceipts
	s.dicom = append(s.dicom, event{Kind: "upload", Sequence: expected.Index, Started: start, Finished: time.Now(), Bytes: count, UploadMS: float64(upload.Sub(start)) / 1e6, ReceiptMS: float64(time.Since(upload)) / 1e6})
	s.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	jsonResponse(w, map[string]any{"status": "accepted", "receiptId": fmt.Sprintf("perf-receipt-%d", seq)})
}

func (s *cloudServer) ingestHL7(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/hl7-v2" || r.Header.Get("Idempotency-Key") == "" {
		s.bad(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.Profile.HL7Limit)+1))
	if err != nil {
		return
	}
	seq, err := parseControlID(body, "hl7")
	if err != nil || !bytes.Equal(body, synthetic.HL7(controlID("hl7", seq), s.cfg.Profile.HL7Bytes)) {
		s.bad(w)
		return
	}
	s.Lock()
	s.snapshot.HL7Accepted++
	s.Unlock()
	w.Header().Set("Content-Type", "application/hl7-v2")
	w.WriteHeader(200)
	_, _ = w.Write(synthetic.ACK("AA", controlID("hl7", seq), "perf-ingest-ack"))
}

func (s *cloudServer) control(w http.ResponseWriter, r *http.Request) {
	s.Lock()
	defer s.Unlock()
	if r.Method == http.MethodDelete {
		w.WriteHeader(204)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/sessions") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		jsonResponse(w, map[string]any{"type": "ready", "sessionId": "perf-session", "connectorId": "perf-relay", "ingestMode": "PRODUCTION", "transports": map[string]any{
			"dicom": map[string]string{"url": s.cfg.Origin + "/v1/relay/ingest/dicom", "contentType": "application/dicom"},
			"hl7":   map[string]string{"url": s.cfg.Origin + "/v1/relay/ingest/hl7", "contentType": "application/hl7-v2"}}})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/poll") {
		s.snapshot.Polls++
		for _, job := range s.jobs {
			if job.Confirmed {
				if job.Event.Finished.IsZero() {
					// Serial Relay polls again after resolving its result request.
					// Include that round trip in completion instead of timestamping
					// success before the cloud response reaches Relay.
					job.Event.Finished = time.Now()
				}
				continue
			}
			if job.Claim > 0 && time.Now().Before(job.Expires) {
				w.WriteHeader(204)
				return
			}
			job.Claim++
			job.Token = fmt.Sprintf("perf-token-%d-%d", job.Event.Sequence, job.Claim)
			job.Expires = time.Now().Add(time.Minute)
			if job.Event.Started.IsZero() {
				job.Event.Started = time.Now()
			}
			jsonResponse(w, map[string]any{"type": "report", "deliveryId": fmt.Sprintf("delivery-%d", job.Event.Sequence), "token": job.Token, "messageControlId": controlID("report", job.Event.Sequence), "payload": string(job.Payload), "payloadSha256": hash(job.Payload), "claimExpiresAt": job.Expires})
			return
		}
		w.WriteHeader(204)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/result") {
		s.snapshot.ResultRequests++
		var result struct {
			Token   string `json:"token"`
			Outcome string `json:"outcome"`
			ACK     string `json:"ackCode"`
			Error   string `json:"error"`
		}
		if decodeRequest(r, &result) != nil {
			s.snapshot.Errors++
			http.Error(w, "invalid result", 400)
			return
		}
		encoded, _ := json.Marshal(result)
		for _, job := range s.jobs {
			if job.Token != result.Token {
				continue
			}
			if !strings.HasSuffix(r.URL.Path, fmt.Sprintf("/delivery-%d/result", job.Event.Sequence)) {
				s.snapshot.Errors++
				http.Error(w, "wrong delivery", 400)
				return
			}
			if job.Confirmed {
				if !bytes.Equal(encoded, job.Result) {
					s.snapshot.Errors++
					http.Error(w, "changed result", 400)
					return
				}
				jsonResponse(w, map[string]bool{"ok": true})
				return
			}
			if !time.Now().Before(job.Expires) {
				http.Error(w, "expired claim", 409)
				return
			}
			if s.behavior.Report == "expired-claim" && job.Claim == 1 {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "synthetic result outage", 503)
				return
			}
			if result.Outcome == "accepted" && (result.ACK != "AA" || job.RISCode != "AA" || job.RISSends == 0) {
				s.snapshot.Errors++
				http.Error(w, "unconfirmed RIS acceptance", 400)
				return
			}
			if result.Outcome != "accepted" && result.Outcome != "failed" {
				s.snapshot.Errors++
				http.Error(w, "invalid outcome", 400)
				return
			}
			job.Confirmed = true
			job.Payload = nil // Only pending claims need retained report bytes.
			job.Result = encoded
			job.Event.Attempts = job.RISSends
			if result.Outcome != "accepted" {
				job.Event.Error = "ris_delivery_failed"
			} else {
				s.snapshot.ReportsConfirmed++
			}
			if s.behavior.Report == "lost-result" {
				panic(http.ErrAbortHandler)
			}
			jsonResponse(w, map[string]bool{"ok": true})
			return
		}
		http.Error(w, "superseded claim", 409)
		return
	}
	http.NotFound(w, r)
}

func (s *cloudServer) admin(w http.ResponseWriter, r *http.Request) {
	s.Lock()
	defer s.Unlock()
	switch r.URL.Path {
	case "/health":
		jsonResponse(w, map[string]bool{"ok": true})
	case "/state":
		snap := s.snapshot
		snap.Events = append([]event(nil), s.dicom...)
		for _, job := range s.jobs {
			snap.Events = append(snap.Events, job.Event)
		}
		jsonResponse(w, snap)
	case "/receipt":
		index, err := strconv.Atoi(r.URL.Query().Get("index"))
		if err != nil {
			http.Error(w, "invalid fixture index", 400)
			return
		}
		var count int
		var last event
		for _, e := range s.dicom {
			if e.Sequence == index {
				count++
				last = e
			}
		}
		jsonResponse(w, struct {
			Count int
			Last  event
		}{count, last})
	case "/reset":
		if s.snapshot.ActiveUploads != 0 {
			http.Error(w, "uploads still active", 409)
			return
		}
		s.snapshot = cloudSnapshot{}
		s.jobs = nil
		s.dicom = nil
		s.behavior = behavior{}
		s.cfg.Calibration = false
		jsonResponse(w, map[string]bool{"ok": true})
	case "/enqueue":
		var e event
		if decodeRequest(r, &e) != nil {
			http.Error(w, "invalid job", 400)
			return
		}
		for _, j := range s.jobs {
			if j.Event.Sequence == e.Sequence {
				http.Error(w, "duplicate sequence", 409)
				return
			}
		}
		payload := synthetic.HL7(controlID("report", e.Sequence), s.cfg.Profile.ReportBytes)
		e.Bytes = int64(len(payload))
		s.jobs = append(s.jobs, &reportJob{Event: e, Payload: payload})
		jsonResponse(w, map[string]bool{"ok": true})
	case "/behavior":
		if r.Method == http.MethodPost {
			if decodeRequest(r, &s.behavior) != nil {
				http.Error(w, "invalid behavior", 400)
				return
			}
		}
		jsonResponse(w, s.behavior)
	case "/ris":
		var observed struct {
			Sequence     int
			Digest, Code string
		}
		if decodeRequest(r, &observed) != nil {
			http.Error(w, "invalid RIS evidence", 400)
			return
		}
		for _, j := range s.jobs {
			if j.Event.Sequence != observed.Sequence {
				continue
			}
			if observed.Digest != hash(j.Payload) {
				s.snapshot.Errors++
				http.Error(w, "changed report", 400)
				return
			}
			j.RISSends++
			j.RISCode = observed.Code
			jsonResponse(w, map[string]bool{"ok": true})
			return
		}
		// Calibration sends directly to the RIS before any cloud job exists.
		if s.cfg.Calibration {
			jsonResponse(w, map[string]bool{"ok": true})
			return
		}
		http.Error(w, "unknown report", 400)
	default:
		http.NotFound(w, r)
	}
}

func serveCloud(ctx context.Context, cfg workerConfig, dir string) error {
	s := &cloudServer{cfg: cfg}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			jsonResponse(w, map[string]bool{"ok": true})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+cfg.Credential {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/v1/relay/ingest/dicom":
			s.ingestDICOM(w, r)
		case "/v1/relay/ingest/hl7":
			s.ingestHL7(w, r)
		default:
			s.control(w, r)
		}
	})
	cloud := &http.Server{Addr: ":8443", Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 * 1024}
	admin := &http.Server{Addr: ":8080", Handler: http.HandlerFunc(s.admin), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	errCh := make(chan error, 2)
	go func() {
		errCh <- cloud.ListenAndServeTLS(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "server-key.pem"))
	}()
	go func() { errCh <- admin.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		_ = cloud.Close()
		_ = admin.Close()
		return err
	}
	_ = cloud.Close()
	return admin.Close()
}

func adminCall(ctx context.Context, cfg workerConfig, path string, body, result any) error {
	var data []byte
	var err error
	method := http.MethodGet
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
		method = http.MethodPost
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, cfg.Admin+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("support request unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("support request returned HTTP %d", resp.StatusCode)
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 32*1024*1024)).Decode(result)
	}
	return nil
}

func serveRIS(ctx context.Context, cfg workerConfig) error {
	l, err := net.Listen("tcp", ":2576")
	if err != nil {
		return err
	}
	defer l.Close()
	go func() { <-ctx.Done(); _ = l.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { _ = c.Close() })
			defer stop()
			_ = c.SetDeadline(time.Now().Add(70 * time.Second))
			payload, err := synthetic.ReadFrame(bufio.NewReader(c), 64*1024)
			if err != nil {
				return
			}
			seq, err := parseControlID(payload, "report")
			if err != nil || !bytes.Equal(payload, synthetic.HL7(controlID("report", seq), cfg.Profile.ReportBytes)) {
				return
			}
			var b behavior
			if adminCall(ctx, cfg, "/behavior", nil, &b) != nil {
				return
			}
			delay := cfg.Profile.ACK
			if b.ACKMillis > 0 {
				delay = b.ACKMillis
			}
			if !waitUntil(ctx, time.Now().Add(millis(delay))) {
				return
			}
			code := "AA"
			switch b.Report {
			case "negative":
				code = "AE"
			case "malformed":
				code = "bad"
			case "missing":
				code = ""
			}
			// Record the exact payload and application ACK before releasing it onto
			// the wire; Relay must still receive and correlate that ACK itself.
			if adminCall(ctx, cfg, "/ris", map[string]any{"Sequence": seq, "Digest": hash(payload), "Code": code}, nil) != nil {
				return
			}
			if b.Report == "missing" {
				// Receiving bytes counts as a RIS send even when no ACK follows.
				_ = waitUntil(ctx, time.Now().Add(25*time.Second))
				return
			}
			ack := synthetic.ACK(code, controlID("report", seq), "perf-ris-ack")
			if b.Report == "malformed" {
				ack = []byte("malformed synthetic ACK")
			}
			_, _ = c.Write(synthetic.Frame(ack))
		}()
	}
}

func readWorker(dir string) (workerConfig, error) {
	var cfg workerConfig
	b, err := os.ReadFile(filepath.Join(dir, "worker.json"))
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	return cfg, err
}

func waitUntil(ctx context.Context, at time.Time) bool {
	t := time.NewTimer(time.Until(at))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
