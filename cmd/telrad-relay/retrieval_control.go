package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

type retrievalProgress struct {
	received atomic.Int64
	uploaded atomic.Int64
	failed   atomic.Int64
}

func (p *retrievalProgress) snapshot() map[string]int64 {
	return map[string]int64{"received": min(p.received.Load(), 1000000), "uploaded": min(p.uploaded.Load(), 1000000), "failed": min(p.failed.Load(), 1000000)}
}

type retrievalClaim struct {
	Type              string    `json:"type"`
	JobID             string    `json:"jobId"`
	AttemptID         string    `json:"attemptId"`
	Token             string    `json:"token"`
	ClaimExpiresAt    time.Time `json:"claimExpiresAt"`
	RenewAfterSeconds int       `json:"renewAfterSeconds"`
	Version           int       `json:"version"`
	ReadinessRule     string    `json:"readinessRule"`
	Permit            string    `json:"permit"`
}

func (c retrievalClaim) valid() bool {
	return c.Type == "retrieval" && retrieval.Opaque(c.JobID) && retrieval.Opaque(c.AttemptID) && validOpaqueID(c.Token) && c.Version == 2 && c.ReadinessRule == "PACS_OR_ORDER" && c.RenewAfterSeconds == 30 && c.ClaimExpiresAt.After(time.Now()) && c.ClaimExpiresAt.Before(time.Now().Add(5*time.Minute+5*time.Second)) && len(c.Permit) <= retrieval.MaxPermitBytes
}
func (c retrievalClaim) body() map[string]any {
	return map[string]any{"attemptId": c.AttemptID, "token": c.Token}
}
func startRetrievalSession(ctx context.Context, cfg *config, ready readyMessage, sessionURL string, client *http.Client, provider *credentialProvider, work *workDrainer, status *runtimeStatusManager) func() {
	if !retrievalEnabledLocal(cfg) || ready.IngestMode != "PRODUCTION" || ready.ConnectorID != cfg.Retrieval.ConnectorID {
		return func() {}
	}
	child, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); pollRetrievals(child, cfg, sessionURL, client, provider, work, status) }()
	return func() { cancel(); <-done }
}
func pollRetrievals(ctx context.Context, cfg *config, sessionURL string, client *http.Client, provider *credentialProvider, work *workDrainer, status *runtimeStatusManager) {
	// Dedicated transport: no cloud Authorization or redirect following can leak
	// onto a PACS connection. One serial worker bounds retrieval concurrency.
	pacsClients := newProtocolClients(cfg)
	pacsClients.transport.Proxy = nil
	defer pacsClients.transport.CloseIdleConnections()
	credential := provider.Current()
	for ctx.Err() == nil {
		if provider.Current() != credential || status.AuthenticationRequired() {
			return
		}
		if _, e := currentRetrievalConfig(cfg); e != nil {
			status.SetRetrievalState("local_policy_rejected")
			return
		}
		var claim retrievalClaim
		code, delay, e := controlRequest(ctx, client, provider, http.MethodPost, sessionURL+"/retrievals/poll", struct{}{}, &claim)
		if code == 401 || code == 403 || code == 409 {
			controlFailure(status, code)
			return
		}
		if e == nil && code == 200 && claim.valid() {
			if ctx.Err() != nil || !work.Start() {
				return
			}
			status.SetRetrievalState("active")
			func() {
				defer work.Done()
				executeRetrieval(ctx, cfg, sessionURL, claim, pacsClients.secure, client, provider, status)
			}()
			status.SetRetrievalState("available")
			continue
		}
		if e == nil && code == 204 {
			status.SetRetrievalState("available")
		} else {
			status.SetRetrievalState("control_unavailable")
		}
		if delay < controlPollInterval {
			delay = controlPollInterval
		}
		if !waitForControl(ctx, nil, delay) {
			return
		}
	}
}

// Retries preserve the exact attempt/session/token and body, including after a
// lost committed response. A stale or conflicting request never changes owners.
func retrievalSubmission(ctx context.Context, client *http.Client, provider *credentialProvider, status *runtimeStatusManager, address string, body any, out any, limit int) error {
	data, e := json.Marshal(body)
	if e != nil || len(data) > limit {
		return retrieval.ErrPolicy
	}
	for ctx.Err() == nil {
		var raw json.RawMessage
		code, delay, e := controlRequest(ctx, client, provider, http.MethodPost, address, json.RawMessage(data), &raw)
		if e == nil && code == 200 {
			var object map[string]json.RawMessage
			if retrieval.StrictJSON(raw, &object) != nil || string(object["ok"]) != "true" || retrieval.StrictJSON(raw, out) != nil {
				e = errors.New("invalid_control_response")
			}
		}
		if e == nil && code == 200 {
			return nil
		}
		if code == 401 || code == 403 {
			status.SetAuthenticationAttention(true)
			return errors.New("lease_lost")
		}
		if code >= 400 && code < 500 && code != 408 && code != 429 {
			return errors.New("lease_lost")
		}
		if e == nil {
			return errors.New("invalid_control_response")
		}
		if delay < time.Second {
			delay = time.Second
		}
		if !waitForControl(ctx, nil, delay) {
			break
		}
	}
	return errors.New("lease_lost")
}
func executeRetrieval(parent context.Context, cfg *config, sessionURL string, claim retrievalClaim, pacsClient, cloudClient *http.Client, provider *credentialProvider, status *runtimeStatusManager) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	leaseTimer := time.AfterFunc(time.Until(claim.ClaimExpiresAt), cancel)
	defer leaseTimer.Stop()
	base := sessionURL + "/retrievals/" + url.PathEscape(claim.JobID)
	credential := provider.Current()
	initial, e := currentRetrievalConfig(cfg)
	var originalPACS retrievalPACS
	if e == nil {
		_, originalPACS, e = authorizeRetrieval(initial, claim.Permit)
	}
	recheck := func() error {
		if ctx.Err() != nil || provider.Current() != credential || status.AuthenticationRequired() {
			return errors.New("lease_lost")
		}
		latest, e := currentRetrievalConfig(cfg)
		if e != nil {
			return e
		}
		_, pacs, e := authorizeRetrieval(latest, claim.Permit)
		if e != nil {
			return e
		}
		// Immutable PACS scope and endpoint cannot change in the middle of an
		// attempt, even if the operator reused an ID accidentally.
		if !reflect.DeepEqual(pacs, originalPACS) {
			return retrieval.ErrPolicy
		}
		return nil
	}
	var progress retrievalProgress
	done := make(chan struct{})
	go func() {
		defer close(done)
		check := time.NewTicker(time.Second)
		defer check.Stop()
		renew := time.NewTimer(30 * time.Second)
		defer renew.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-check.C:
				if recheck() != nil {
					cancel()
					return
				}
			case <-renew.C:
				var reply struct {
					OK             bool      `json:"ok"`
					ClaimExpiresAt time.Time `json:"claimExpiresAt"`
				}
				renewBody := claim.body()
				renewBody["progress"] = progress.snapshot()
				code, delay, err := controlRequest(ctx, cloudClient, provider, http.MethodPost, base+"/renew", renewBody, &reply)
				if err == nil && code == 200 && reply.OK && reply.ClaimExpiresAt.After(time.Now()) && reply.ClaimExpiresAt.Before(time.Now().Add(5*time.Minute+5*time.Second)) {
					if !leaseTimer.Stop() {
						cancel()
						return
					}
					leaseTimer.Reset(time.Until(reply.ClaimExpiresAt))
					renew.Reset(30 * time.Second)
				} else if code == 401 || code == 403 || code == 409 || (code >= 400 && code < 500 && code != 408 && code != 429) {
					if code == 401 || code == 403 {
						status.SetAuthenticationAttention(true)
					}
					cancel()
					return
				} else {
					if delay < 10*time.Second {
						delay = 10 * time.Second
					}
					renew.Reset(delay)
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	result := retrievalResult{}
	if e != nil {
		result.Outcome = retrievalOutcome(e)
	} else {
		p, _, err := authorizeRetrieval(initial, claim.Permit)
		if err == nil {
			err = recheck()
		}
		var selected []string
		if err == nil {
			if originalPACS.Adapter == "dimse-find-get-v1" {
				selected, err = queryDIMSEAccession(ctx, originalPACS, p)
			} else {
				selected, err = queryAccession(ctx, pacsClient, originalPACS, p)
			}
		}
		if err == nil {
			body := claim.body()
			body["studyInstanceUids"] = selected
			var confirmation struct {
				OK                bool     `json:"ok"`
				StudyInstanceUIDs []string `json:"studyInstanceUids"`
			}
			err = retrievalSubmission(ctx, cloudClient, provider, status, base+"/studies", body, &confirmation, 8*1024)
			if err == nil && (!confirmation.OK || !slices.Equal(confirmation.StudyInstanceUIDs, selected)) {
				err = errors.New("partial_transfer")
			}
		}
		total := 0
		if err == nil {
			for _, study := range selected {
				var transferred retrievalStudyResult
				if originalPACS.Adapter == "dimse-find-get-v1" {
					transferred, err = retrieveCGET(ctx, cloudClient, cfg, provider, status, originalPACS, p, study, claim.AttemptID, recheck, &progress)
				} else {
					transferred, err = retrieveWADO(ctx, pacsClient, cloudClient, cfg, provider, status, originalPACS, p, study, claim.AttemptID, recheck, &progress)
				}
				if err != nil {
					progress.failed.Add(1)
					break
				}
				total += transferred.UniqueInstanceCount
				if total > 1000000 {
					err = retrieval.ErrPolicy
					break
				}
				result.Studies = append(result.Studies, transferred)
			}
		}
		if err != nil {
			result = retrievalResult{Outcome: retrievalOutcome(err)}
		} else {
			zero := 0
			result.Outcome = "uploaded"
			result.RetrievalMethod = "WADO_RS"
			if originalPACS.Adapter == "dimse-find-get-v1" {
				result.RetrievalMethod = "C_GET"
			}
			result.OutstandingUploads = &zero
		}
	}
	if ctx.Err() != nil {
		return
	} // Expiry/reassignment owns recovery; never false success.
	body := claim.body()
	body["result"] = result
	var confirmation struct {
		OK bool `json:"ok"`
	}
	_ = retrievalSubmission(ctx, cloudClient, provider, status, base+"/result", body, &confirmation, 32*1024)
}
func retrievalOutcome(e error) string {
	for _, s := range []string{"not_found", "pacs_not_ready", "completion_query_failed", "partial_transfer", "network_timeout", "pacs_unavailable", "pacs_rejected", "ambiguous_identity", "identity_mismatch", "untrusted_key", "local_policy_rejected", "relay_draining"} {
		if e.Error() == s {
			return s
		}
	}
	return "local_policy_rejected"
}
