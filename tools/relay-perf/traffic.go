package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

type trafficResult struct {
	Start  time.Time `json:"start"`
	Events []event   `json:"events"`
	Error  string    `json:"error,omitempty"`
}

func trafficWorkers(p profile, kind string, calibration bool) int {
	n := map[string]int{"dicom": p.DICOMConnections, "hl7": p.HL7Connections, "report": 1}[kind]
	if calibration {
		return max(32, n)
	}
	return n
}

func planned(p profile, start time.Time, kind string, calibration bool) []event {
	if len(p.Mix) > 0 && (kind == "dicom" || kind == "study") {
		return mixedPlanned(p, start, kind, calibration)
	}
	rate := map[string]float64{"dicom": p.DICOMRate, "hl7": p.HL7Rate, "report": p.ReportRate}[kind]
	phases := []struct {
		name                     string
		offset, duration, factor float64
	}{{"nominal", p.Idle, p.Nominal, 1}, {"headroom", p.Idle + p.Nominal, p.Headroom, p.Factor}}
	if calibration {
		phases = []struct {
			name                     string
			offset, duration, factor float64
		}{{"calibration", 0, p.calibrationSeconds(), 2 * p.Factor}}
	}
	var events []event
	for _, phase := range phases {
		for i := 0; i < int(math.Ceil(rate*phase.factor*phase.duration)); i++ {
			events = append(events, event{Kind: kind, Sequence: len(events), Phase: phase.name, Scheduled: start.Add(seconds(phase.offset + float64(i)/(rate*phase.factor)))})
		}
	}
	return events
}

// schedule never waits for a free worker. A full queue creates an explicit
// failed arrival, preserving the offered workload and its original timestamp.
func schedule(ctx context.Context, events []event, workers int, perform func(context.Context, event) event, log *eventLog) {
	jobs := make(chan event, max(1024, len(events)))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				log.add(perform(ctx, e))
			}
		}()
	}
	for _, e := range events {
		if !waitUntil(ctx, e.Scheduled) {
			e.Error = "cancelled_before_start"
			e.Finished = time.Now()
			log.add(e)
			continue
		}
		select {
		case jobs <- e:
		default:
			e.Error = "offered_queue_full"
			e.Finished = time.Now()
			log.add(e)
		}
	}
	close(jobs)
	wg.Wait()
}

func tlsClient(dir string) (*http.Client, error) {
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid synthetic CA")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, MaxIdleConns: 256, MaxIdleConnsPerHost: 128}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 60 * time.Second}, nil
}

func sendHTTP(ctx context.Context, client *http.Client, cfg workerConfig, kind string, body io.Reader, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Origin+"/v1/relay/ingest/"+kind, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Credential)
	req.Header.Set("X-Telrad-Protocol-Version", "1")
	req.Header.Set("Content-Type", map[string]string{"dicom": "application/dicom", "hl7": "application/hl7-v2"}[kind])
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != map[string]int{"hl7": 200, "dicom": 201}[kind] {
		return nil, errors.New("ingest rejected")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err == nil && kind == "dicom" {
		var receipt struct {
			Status    string
			ReceiptID string `json:"receiptId"`
		}
		if json.Unmarshal(b, &receipt) != nil || receipt.Status != "accepted" || receipt.ReceiptID == "" {
			return nil, errors.New("calibration receipt invalid")
		}
	}
	return b, err
}

// Support calibration measures sustained capacity after connection setup. These
// health requests carry no workload and never warm Relay's own connections.
func warmCalibration(ctx context.Context, client *http.Client, cfg workerConfig) error {
	var wg sync.WaitGroup
	errors := make(chan error, trafficWorkers(cfg.Profile, "dicom", true))
	for range cap(errors) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Origin+"/health", nil)
			if err == nil {
				var response *http.Response
				response, err = client.Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
					_ = response.Body.Close()
					if response.StatusCode != http.StatusOK {
						err = fmt.Errorf("calibration health status %d", response.StatusCode)
					}
				}
			}
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

// Preserve elapsed monotonic time when timestamps cross the JSON boundary.
// time.Time serialization drops its monotonic component; wall-clock adjustment
// must not make a correctly scheduled operation appear to start early.
func elapsedTimestamp(start, now time.Time) time.Time { return start.Add(now.Sub(start)) }

func runTraffic(ctx context.Context, cfg workerConfig, dir string) trafficResult {
	result := trafficResult{Start: time.Now().Add(2 * time.Second)}
	client, err := tlsClient(dir)
	if err != nil {
		result.Error = "invalid_support_tls"
		return result
	}
	defer client.CloseIdleConnections()
	// Prefetch bounded synthetic inputs outside the measurement interval. This
	// prevents Docker Desktop bind-mount I/O from becoming the traffic generator's
	// throughput limit. This memory belongs to the supporting container.
	cached := make(map[int][]byte)
	var cachedBytes int64
	for _, f := range cfg.Fixtures {
		if cachedBytes+f.Offset+f.DatasetBytes > 128<<20 {
			continue
		}
		b, err := os.ReadFile(fixturePath(cfg, dir, f))
		if err != nil {
			result.Error = "fixture_unavailable"
			return result
		}
		if hash(b) != f.FileDigest {
			result.Error = "fixture_integrity_failed"
			return result
		}
		cached[f.Index] = b
		cachedBytes += int64(len(b))
	}
	var idle []net.Conn
	if !cfg.Calibration {
		for range cfg.Profile.IdleHL7 {
			c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Relay, "2575"))
			if err != nil {
				result.Error = "idle_connection_failed"
				break
			}
			idle = append(idle, c)
		}
	}
	defer func() {
		for _, c := range idle {
			_ = c.Close()
		}
	}()
	// Allocate one persistent connection per worker, not per message.
	type link struct {
		sync.Mutex
		c     net.Conn
		r     *bufio.Reader
		class string
	}
	dicom := make(chan *link, cfg.Profile.DICOMConnections)
	hl7 := make(chan *link, cfg.Profile.HL7Connections)
	var links []*link
	for i := 0; i < cfg.Profile.DICOMConnections+cfg.Profile.HL7Connections; i++ {
		l := &link{}
		links = append(links, l)
		if i < cfg.Profile.DICOMConnections {
			dicom <- l
		} else {
			hl7 <- l
		}
	}
	defer func() {
		for _, l := range links {
			l.Lock()
			if l.c != nil {
				_ = l.c.Close()
			}
			l.Unlock()
		}
	}()
	stop := context.AfterFunc(ctx, func() {
		for _, l := range links {
			l.Lock()
			if l.c != nil {
				_ = l.c.Close()
			}
			l.Unlock()
		}
	})
	defer stop()
	fixtureLocks := make([]sync.Mutex, len(cfg.Fixtures))
	if cfg.Calibration {
		if err := warmCalibration(ctx, client, cfg); err != nil {
			result.Error = "calibration_connection_setup_failed"
			return result
		}
	}
	result.Start = time.Now().Add(2 * time.Second)
	perform := func(ctx context.Context, e event) event {
		e.Started = elapsedTimestamp(result.Start, time.Now())
		finish := func(err error) event {
			e.Finished = elapsedTimestamp(result.Start, time.Now())
			if err != nil {
				e.Error = e.Kind + "_exchange_failed"
			}
			return e
		}
		if ctx.Err() != nil {
			return finish(ctx.Err())
		}
		switch e.Kind {
		case "report":
			if !cfg.Calibration {
				if err := adminCall(ctx, cfg, "/enqueue", e, nil); err != nil {
					return finish(err)
				}
				// Confirmed results are collected from the cloud after draining.
				return e
			}
			c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", cfg.RIS)
			if err != nil {
				return finish(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			payload := synthetic.Report(controlID("report", e.Sequence), cfg.Profile.ReportBytes)
			_, err = c.Write(synthetic.Frame(payload))
			if err != nil {
				return finish(err)
			}
			ack, err := synthetic.ReadFrame(bufio.NewReader(c), 64*1024)
			if err == nil && !bytes.Equal(ack, synthetic.ACK("AA", controlID("report", e.Sequence), "perf-ris-ack")) {
				err = errors.New("invalid RIS ACK")
			}
			e.Bytes = int64(len(payload))
			return finish(err)
		case "hl7":
			payload := synthetic.HL7(controlID("hl7", e.Sequence), cfg.Profile.HL7Bytes)
			e.Bytes = int64(len(payload))
			var ack []byte
			var err error
			if cfg.Calibration {
				ack, err = sendHTTP(ctx, client, cfg, "hl7", bytes.NewReader(payload), fmt.Sprintf("perf-calibration-%d", e.Sequence))
			} else {
				l := <-hl7
				defer func() { hl7 <- l }()
				l.Lock()
				if l.c == nil {
					l.c, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Relay, "2575"))
					if err == nil {
						l.r = bufio.NewReaderSize(l.c, 32*1024)
					}
				}
				c, r := l.c, l.r
				l.Unlock()
				if err != nil {
					return finish(err)
				}
				_ = c.SetDeadline(time.Now().Add(60 * time.Second))
				_, err = c.Write(synthetic.Frame(payload))
				if err == nil {
					ack, err = synthetic.ReadFrame(r, 64*1024)
				}
				if err != nil {
					_ = c.Close()
					l.Lock()
					l.c = nil
					l.Unlock()
				}
			}
			if err == nil && !bytes.Equal(ack, synthetic.ACK("AA", controlID("hl7", e.Sequence), "perf-ingest-ack")) {
				err = errors.New("changed cloud ACK")
			}
			return finish(err)
		case "dicom":
			index := e.Sequence % len(cfg.Fixtures)
			if len(cfg.Profile.Mix) > 0 {
				index = e.Fixture
			}
			f := cfg.Fixtures[index]
			if !cfg.Calibration {
				fixtureLocks[f.Index].Lock()
				defer fixtureLocks[f.Index].Unlock()
			}
			var file io.ReadSeeker
			var err error
			if data, ok := cached[f.Index]; ok {
				file = bytes.NewReader(data)
			} else {
				disk, openErr := os.Open(fixturePath(cfg, dir, f))
				if openErr != nil {
					return finish(openErr)
				}
				defer disk.Close()
				file = disk
			}
			e.Bytes = f.DatasetBytes
			if cfg.Calibration {
				_, err := sendHTTP(ctx, client, cfg, "dicom", file, "")
				return finish(err)
			}
			var before struct{ Count int }
			if err := adminCall(ctx, cfg, fmt.Sprintf("/receipt?index=%d", f.Index), nil, &before); err != nil {
				return finish(err)
			}
			l := <-dicom
			defer func() { dicom <- l }()
			l.Lock()
			if l.c != nil && l.class != f.storageClass() {
				_ = l.c.SetDeadline(time.Now().Add(5 * time.Second))
				if err := synthetic.WritePDU(l.c, 5, make([]byte, 4)); err == nil {
					_, _, _ = synthetic.ReadPDU(l.c)
				}
				_ = l.c.Close()
				l.c = nil
			}
			newAssociation := l.c == nil
			if newAssociation {
				l.c, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Relay, "11112"))
			}
			c := l.c
			l.Unlock()
			if err != nil {
				return finish(err)
			}
			if newAssociation {
				_ = c.SetDeadline(time.Now().Add(60 * time.Second))
				// Cancellation must be able to close the socket while association
				// negotiation is waiting on the peer.
				if err := synthetic.AssociateStorage(c, f.storageClass(), cfg.Profile.Syntax); err != nil {
					_ = c.Close()
					l.Lock()
					l.c = nil
					l.Unlock()
					return finish(err)
				}
				l.Lock()
				l.class = f.storageClass()
				l.Unlock()
			}
			_ = c.SetDeadline(time.Now().Add(60 * time.Second))
			_, err = file.Seek(f.Offset, io.SeekStart)
			if err == nil {
				err = synthetic.SendStorage(c, f.storageClass(), f.Instance, uint16(e.Sequence%65535+1), file, f.DatasetBytes, cfg.Profile.PDU, true, true)
			}
			if err == nil {
				var status uint16
				status, err = synthetic.StoreResponse(c, uint16(e.Sequence%65535+1))
				if err == nil && status != 0 {
					err = errors.New("store rejected")
				}
			}
			finished := time.Now()
			if err != nil {
				_ = c.Close()
				l.Lock()
				l.c = nil
				l.Unlock()
				return finish(err)
			}
			var after struct {
				Count int
				Last  event
			}
			if err := adminCall(ctx, cfg, fmt.Sprintf("/receipt?index=%d", f.Index), nil, &after); err != nil {
				return finish(err)
			}
			if after.Count != before.Count+1 || after.Last.Finished.After(finished) {
				return finish(errors.New("C-STORE preceded fresh receipt"))
			}
			e.Finished = elapsedTimestamp(result.Start, finished)
			e.UploadMS = after.Last.UploadMS
			e.ReceiptMS = after.Last.ReceiptMS
			return e
		}
		return finish(errors.New("unknown traffic"))
	}
	var log eventLog
	var wg sync.WaitGroup
	for _, kind := range []string{"dicom", "hl7", "report"} {
		workers := trafficWorkers(cfg.Profile, kind, cfg.Calibration)
		events := planned(cfg.Profile, result.Start, kind, cfg.Calibration)
		wg.Add(1)
		go func() { defer wg.Done(); schedule(ctx, events, workers, perform, &log) }()
	}
	wg.Wait()
	end := result.Start.Add(cfg.Profile.total())
	if cfg.Calibration {
		end = result.Start.Add(seconds(cfg.Profile.calibrationSeconds() + 5))
	}
	_ = waitUntil(ctx, end)
	result.Events = log.snapshot()
	if !cfg.Calibration {
		var cloud cloudSnapshot
		if adminCall(ctx, cfg, "/state", nil, &cloud) != nil {
			result.Error = "cloud_measurements_unavailable"
		} else {
			confirmed := map[int]event{}
			for _, e := range cloud.Events {
				if e.Kind == "report" {
					confirmed[e.Sequence] = e
				}
			}
			for i, e := range result.Events {
				if e.Kind == "report" && e.Error == "" {
					if c, ok := confirmed[e.Sequence]; ok {
						result.Events[i] = c
					}
				}
			}
		}
		if len(cfg.Profile.Mix) > 0 {
			result.Events = append(result.Events, mixedStudyEvents(result.Events, cfg.Profile, result.Start)...)
		} else {
			result.Events = append(result.Events, studyEvents(result.Events, cfg.Profile.Instances)...)
		}
	}
	sortEvents(result.Events)
	return result
}

func studyEvents(events []event, size int) []event {
	groups := map[int][]event{}
	for _, e := range events {
		if e.Kind == "dicom" {
			groups[e.Sequence/size] = append(groups[e.Sequence/size], e)
		}
	}
	var out []event
	for index, members := range groups {
		e := event{Kind: "study", Sequence: index, Phase: members[0].Phase, Scheduled: members[0].Scheduled, Started: members[0].Started}
		for _, m := range members {
			if m.Scheduled.Before(e.Scheduled) {
				e.Scheduled = m.Scheduled
			}
			if !m.Started.IsZero() && (e.Started.IsZero() || m.Started.Before(e.Started)) {
				e.Started = m.Started
			}
			if m.Finished.After(e.Finished) {
				e.Finished = m.Finished
			}
			if m.Error != "" || m.Finished.IsZero() {
				e.Error = "incomplete_study"
			}
			e.Bytes += m.Bytes
		}
		out = append(out, e)
	}
	return out
}

func trafficToFile(ctx context.Context, cfg workerConfig, dir, out string) error {
	r := runTraffic(ctx, cfg, dir)
	if err := writeJSON(out, r); err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
