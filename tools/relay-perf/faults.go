package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

type faultResult struct {
	Started        time.Time `json:"started"`
	Name           string    `json:"name"`
	Passed         bool      `json:"passed"`
	ElapsedMS      float64   `json:"elapsedMillis"`
	Error          string    `json:"error,omitempty"`
	Attempts       int       `json:"risSends,omitempty"`
	Polls          int       `json:"polls,omitempty"`
	ResultRequests int       `json:"resultRequests,omitempty"`
	ActiveUploads  int       `json:"activeUploads,omitempty"`
}

var reportFaultCases = []string{"immediate", "delayed", "negative", "missing", "malformed", "lost-result", "expired-claim"}
var dicomFaultCases = []string{"slow-progress", "disconnect", "abort", "malformed-pdv", "invalid-context", "unexpected-command", "stall"}

func runFaults(ctx context.Context, cfg workerConfig, dir string) []faultResult {
	var results []faultResult
	for _, mode := range reportFaultCases {
		start := time.Now()
		f := faultResult{Name: mode, Started: start}
		caseCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		if err := adminCall(caseCtx, cfg, "/reset", struct{}{}, nil); err != nil {
			f.Error = "reset_failed"
			results = append(results, f)
			cancel()
			continue
		}
		b := behavior{Report: mode}
		if mode == "delayed" {
			b.ACKMillis = 500
		}
		err := adminCall(caseCtx, cfg, "/behavior", b, nil)
		if err == nil {
			err = adminCall(caseCtx, cfg, "/enqueue", event{Kind: "report", Sequence: 0, Phase: mode, Scheduled: time.Now()}, nil)
		}
		var snapshot cloudSnapshot
		var delivery event
		for err == nil && caseCtx.Err() == nil {
			err = adminCall(caseCtx, cfg, "/state", nil, &snapshot)
			for _, e := range snapshot.Events {
				if e.Kind == "report" {
					delivery = e
				}
			}
			// A lost response must cause an identical result replay, even though
			// the cloud already committed the first result.
			if !delivery.Finished.IsZero() && (mode != "lost-result" || snapshot.ResultRequests >= 2) {
				break
			}
			if !waitUntil(caseCtx, time.Now().Add(100*time.Millisecond)) {
				break
			}
		}
		f.Attempts, f.Polls, f.ResultRequests = delivery.Attempts, snapshot.Polls, snapshot.ResultRequests
		expectedFailure := mode == "negative" || mode == "missing" || mode == "malformed"
		f.Passed = err == nil && !delivery.Finished.IsZero() && snapshot.Errors == 0 && (delivery.Error != "") == expectedFailure
		if mode == "lost-result" {
			f.Passed = f.Passed && delivery.Attempts == 1 && snapshot.ResultRequests >= 2
		}
		if mode == "expired-claim" {
			f.Passed = f.Passed && delivery.Attempts == 2 && snapshot.ResultRequests >= 2
		}
		if !f.Passed {
			f.Error = "report_recovery_contract_failed"
		}
		f.ElapsedMS = float64(time.Since(start)) / 1e6
		deadlineSeconds := 20.0 // Normal application ACK timeout.
		if mode == "expired-claim" || mode == "lost-result" {
			deadlineSeconds = 60
		}
		if f.ElapsedMS > (deadlineSeconds+5)*1000 {
			f.Passed = false
			f.Error = "report_recovery_deadline_exceeded"
		}
		results = append(results, f)
		cancel()
	}
	// Every abort follows a non-final dataset fragment which has reached HTTPS.
	// Repeated cases deliberately leave #16 visible rather than treating a
	// service restart as proof that abandoned uploads recovered by themselves.
	for _, mode := range dicomFaultCases {
		start := time.Now()
		f := faultResult{Name: mode, Started: start}
		caseCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := adminCall(caseCtx, cfg, "/reset", struct{}{}, nil)
		if err != nil {
			f.Error = "precondition_failed_previous_upload_still_active"
		}
		if err == nil {
			err = dicomFault(caseCtx, cfg, dir, mode)
		}
		var state cloudSnapshot
		_ = adminCall(caseCtx, cfg, "/state", nil, &state)
		f.ActiveUploads = state.ActiveUploads
		f.Passed = err == nil && state.ActiveUploads == 0
		if !f.Passed && f.Error == "" {
			f.Error = "upload_recovery_failed_issue_16"
		}
		f.ElapsedMS = float64(time.Since(start)) / 1e6
		results = append(results, f)
		cancel()
	}
	return results
}

func dicomFault(ctx context.Context, cfg workerConfig, dir, mode string) error {
	f := cfg.Fixtures[0]
	if len(cfg.Profile.Mix) > 0 {
		for _, candidate := range cfg.Fixtures {
			if candidate.DatasetBytes >= 512<<10 {
				f = candidate
				break
			}
		}
	}
	if mode == "slow-progress" {
		if err := adminCall(ctx, cfg, "/behavior", behavior{DICOM: "slow-progress"}, nil); err != nil {
			return err
		}
		return recoveryStore(ctx, cfg, dir, f)
	}
	for cycle := 0; cycle < 3; cycle++ {
		if mode == "stall" {
			if err := adminCall(ctx, cfg, "/behavior", behavior{DICOM: "stall"}, nil); err != nil {
				return err
			}
		}
		c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Relay, "11112"))
		if err != nil {
			return err
		}
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if err = synthetic.AssociateStorage(c, f.storageClass(), cfg.Profile.Syntax); err != nil {
			c.Close()
			return err
		}
		file, err := os.Open(fixturePath(cfg, dir, f))
		if err != nil {
			c.Close()
			return err
		}
		_, err = file.Seek(f.Offset, io.SeekStart)
		chunk := make([]byte, min(f.DatasetBytes, 32*1024))
		if err == nil {
			_, err = io.ReadFull(file, chunk)
		}
		file.Close()
		if err == nil {
			err = synthetic.WritePDU(c, 4, synthetic.PDV(synthetic.StorageCommand(f.storageClass(), 1, f.Instance), 3))
		}
		if err == nil {
			err = synthetic.WritePDU(c, 4, synthetic.PDV(chunk, 0))
		}
		if err != nil {
			c.Close()
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		started := false
		for time.Now().Before(deadline) {
			var state cloudSnapshot
			if adminCall(ctx, cfg, "/state", nil, &state) == nil && state.ActiveUploads > 0 {
				started = true
				break
			}
			if !waitUntil(ctx, time.Now().Add(50*time.Millisecond)) {
				break
			}
		}
		if !started {
			c.Close()
			return errors.New("upload did not start")
		}
		faultDeadline := time.Now().Add(10 * time.Second)
		switch mode {
		case "abort":
			err = synthetic.WritePDU(c, 7, []byte{0, 0, 0, 0})
		case "malformed-pdv":
			err = synthetic.WritePDU(c, 4, []byte{0, 0, 0, 1, 1})
		case "invalid-context":
			pdv := synthetic.PDV(chunk, 0)
			pdv[4] = 99
			err = synthetic.WritePDU(c, 4, pdv)
		case "unexpected-command":
			err = synthetic.WritePDU(c, 4, synthetic.PDV(synthetic.StorageCommand(f.storageClass(), 2, f.Instance), 3))
		case "disconnect":
			_ = c.Close()
		case "stall":
			// Fill the transport buffers without completing the dataset. Once the
			// sender blocks, Relay's configured timeout must cancel cloud work.
			stallStart := time.Now()
			_ = c.SetDeadline(stallStart.Add(20 * time.Second))
			for i := 0; i < 2048; i++ {
				if err = synthetic.WritePDU(c, 4, synthetic.PDV(chunk, 0)); err != nil {
					break
				}
			}
			if err == nil || time.Since(stallStart) > 10*time.Second {
				c.Close()
				return errors.New("stalled upload exceeded idle timeout and recovery slack")
			}
		}
		_ = c.Close()
		deadline = faultDeadline
		recovered := false
		for time.Now().Before(deadline) {
			var state cloudSnapshot
			if adminCall(ctx, cfg, "/state", nil, &state) == nil && state.ActiveUploads == 0 {
				recovered = true
				break
			}
			if !waitUntil(ctx, time.Now().Add(100*time.Millisecond)) {
				break
			}
		}
		if !recovered {
			return errors.New("abandoned cloud upload remains active")
		}
		if err := adminCall(ctx, cfg, "/behavior", behavior{}, nil); err != nil {
			return err
		}
		// Recovery requires a subsequent successful, receipt-backed C-STORE.
		if err := recoveryStore(ctx, cfg, dir, f); err != nil {
			return err
		}
	}
	return nil
}

func recoveryStore(ctx context.Context, cfg workerConfig, dir string, f fixture) error {
	var before struct{ Count int }
	if err := adminCall(ctx, cfg, fmt.Sprintf("/receipt?index=%d", f.Index), nil, &before); err != nil {
		return err
	}
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Relay, "11112"))
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := synthetic.AssociateStorage(c, f.storageClass(), cfg.Profile.Syntax); err != nil {
		return err
	}
	file, err := os.Open(fixturePath(cfg, dir, f))
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(f.Offset, io.SeekStart); err != nil {
		return err
	}
	if err := synthetic.SendStorage(c, f.storageClass(), f.Instance, 1, file, f.DatasetBytes, cfg.Profile.PDU, true, true); err != nil {
		return err
	}
	code, err := synthetic.StoreResponse(c, 1)
	finished := time.Now()
	if err != nil || code != 0 {
		return fmt.Errorf("recovery C-STORE failed: %04x", code)
	}
	var after struct {
		Count int
		Last  event
	}
	if err := adminCall(ctx, cfg, fmt.Sprintf("/receipt?index=%d", f.Index), nil, &after); err != nil {
		return err
	}
	if after.Count != before.Count+1 || after.Last.Finished.After(finished) {
		return errors.New("recovery C-STORE preceded its fresh receipt")
	}
	return nil
}
