package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRetrievalLeaseLossCancelsBlockedUpload(t *testing.T) {
	cfg := retrievalTestConfig(t)
	provider := testProvider(t, testCredential('A'))
	status := newRuntimeStatus(cfg.configPath)
	envelope := testSignedReferral(t, cfg)
	entered := make(chan struct{})
	uploadCancelled := make(chan struct{})
	var once sync.Once
	resultCalls := 0
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/studies"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "studyInstanceUids": []string{"1.2.3"}})
		case strings.HasSuffix(r.URL.Path, "/dicom"):
			io.Copy(io.Discard, r.Body)
			once.Do(func() { close(entered) })
			<-r.Context().Done()
			close(uploadCancelled)
		case strings.HasSuffix(r.URL.Path, "/result"):
			resultCalls++
			io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer cloud.Close()
	cfg.Retrieval.PACS[0] = dimseWorkflowPACS(t, func() []string { return []string{"1.2.3"} }, 1)
	setRetrievalCloud(cfg, cloud.URL)
	saveRetrievalTestConfig(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		executeRetrieval(ctx, cfg, cfg.ControlURL+"/sessions/owner", retrievalClaim{JobID: "job", AttemptID: "attempt", Token: "token", ClaimExpiresAt: time.Now().Add(time.Second), Permit: envelope}, cloud.Client(), provider, status)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("upload did not start")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("lease did not stop work")
	}
	select {
	case <-uploadCancelled:
	case <-ctx.Done():
		t.Fatal("upload was not cancelled")
	}
	if resultCalls != 0 {
		t.Fatal("expired upload reported success")
	}
}

func TestRetrievalRenewsLeaseWithProgressDuringUpload(t *testing.T) {
	cfg := retrievalTestConfig(t)
	provider := testProvider(t, testCredential('A'))
	status := newRuntimeStatus(cfg.configPath)
	envelope := testSignedReferral(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	renewed := make(chan struct{})
	var once sync.Once
	var result retrievalResult
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+testCredential('A') || r.Header.Get("X-Telrad-Protocol-Version") != "1" {
			t.Error("missing lease authentication")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/studies"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "studyInstanceUids": []string{"1.2.3"}})
		case strings.HasSuffix(r.URL.Path, "/dicom"):
			io.Copy(io.Discard, r.Body)
			select {
			case <-renewed:
			case <-r.Context().Done():
				return
			}
			// The original lease ends at 32 seconds. Continue beyond it to prove
			// the renewed deadline, not only the HTTP renewal exchange, is honored.
			select {
			case <-time.After(4 * time.Second):
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"status":"accepted","receiptId":"receipt"}`)
		case strings.HasSuffix(r.URL.Path, "/renew"):
			var body struct {
				AttemptID, Token string
				Progress         map[string]int
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.AttemptID != "attempt" || body.Token != "token" || body.Progress["received"] != 1 || body.Progress["uploaded"] != 0 || body.Progress["failed"] != 0 {
				t.Error("invalid renewal/progress")
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "claimExpiresAt": time.Now().Add(5 * time.Minute)})
			once.Do(func() { close(renewed) })
		case strings.HasSuffix(r.URL.Path, "/result"):
			var body struct{ Result retrievalResult }
			json.NewDecoder(r.Body).Decode(&body)
			result = body.Result
			io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer cloud.Close()
	cfg.Retrieval.PACS[0] = dimseWorkflowPACS(t, func() []string { return []string{"1.2.3"} }, 1)
	cfg.Retrieval.PACS[0].RequestTimeoutSeconds = 45
	setRetrievalCloud(cfg, cloud.URL)
	saveRetrievalTestConfig(t, cfg)
	// The upload waits longer than the ordinary 30-second header timeout only
	// in this transport fixture; production PACS/HTTP profiles remain bounded.
	client := cloud.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 40 * time.Second
	client.Transport = transport
	defer transport.CloseIdleConnections()
	executeRetrieval(ctx, cfg, cfg.ControlURL+"/sessions/owner", retrievalClaim{JobID: "job", AttemptID: "attempt", Token: "token", ClaimExpiresAt: time.Now().Add(32 * time.Second), Permit: envelope}, client, provider, status)
	if result.Outcome != "uploaded" || len(result.Studies) != 1 {
		t.Fatalf("renewed transfer outcome=%s", result.Outcome)
	}
}
