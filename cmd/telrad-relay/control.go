package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"time"
)

const controlPollInterval = 3 * time.Second
const controlRequestTimeout = 15 * time.Second

type readyMessage struct {
	Type        string                    `json:"type"`
	SessionID   string                    `json:"sessionId"`
	ConnectorID string                    `json:"connectorId"`
	IngestMode  string                    `json:"ingestMode"`
	Transports  map[string]readyTransport `json:"transports"`
}
type readyTransport struct {
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
}

func validateReady(cfg *config, ready readyMessage) error {
	if ready.Type != "ready" || !validOpaqueID(ready.SessionID) {
		return errors.New("missing session")
	}
	expected := map[string]readyTransport{"dicom": {URL: cfg.DicomURL, ContentType: "application/dicom"}, "hl7": {URL: cfg.HL7URL, ContentType: "application/hl7-v2"}}
	if len(ready.Transports) != len(expected) {
		return errors.New("unexpected transports")
	}
	for name, value := range expected {
		if ready.Transports[name] != value {
			return errors.New("transport mismatch")
		}
	}
	return nil
}

// HTTP success for a report result means the outcome has committed, not merely arrived.
func controlRequest(ctx context.Context, client *http.Client, provider *credentialProvider, method, address string, body, result any) (int, time.Duration, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, 0, errors.New("invalid_control_request")
	}
	requestCtx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, address, bytes.NewReader(data))
	if err != nil {
		return 0, 0, errors.New("invalid_control_url")
	}
	addRelayHeaders(req, provider, "application/json", "")
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, safeNetworkError(err)
	}
	defer resp.Body.Close()
	delay := retryAfterDelay(resp.Header.Get("Retry-After"), time.Now())
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return resp.StatusCode, delay, errors.New("control_request_failed")
	}
	if resp.StatusCode != http.StatusNoContent && result != nil {
		if !mediaTypeEquals(resp.Header.Get("Content-Type"), "application/json") || decodeBoundedJSON(resp.Body, maxCloudResponseBytes, result) != nil {
			return resp.StatusCode, delay, errors.New("invalid_control_response")
		}
	}
	return resp.StatusCode, delay, nil
}

func waitForControl(ctx context.Context, changed <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-changed:
		return true
	case <-timer.C:
		return true
	}
}
func controlFailure(status *runtimeStatusManager, code int) {
	status.SetControlConnected(false)
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		status.SetAuthenticationAttention(true)
	}
}

func superviseControl(ctx, workCtx context.Context, cfg *config, client *http.Client, provider *credentialProvider, credentialChanged <-chan struct{}, work *workDrainer, status *runtimeStatusManager) {
	sessionURL := ""
	backoff := time.Duration(0)
	defer func() {
		status.SetControlConnected(false)
		if sessionURL != "" {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(workCtx), 5*time.Second)
			defer cancel()
			_, _, _ = controlRequest(closeCtx, client, provider, http.MethodDelete, sessionURL, nil, nil)
		}
	}()
	for ctx.Err() == nil {
		var code int
		var delay time.Duration
		var err error
		if sessionURL == "" {
			hostname, _ := os.Hostname()
			hello := map[string]any{"type": "hello", "agentVersion": version, "platform": relayPlatform(), "hostname": hostname, "capabilities": map[string]bool{"dicom": true, "hl7": true, "reportDelivery": true, "httpsIngest": true}}
			var ready readyMessage
			code, delay, err = controlRequest(ctx, client, provider, http.MethodPost, cfg.ControlURL+"/sessions", hello, &ready)
			if err == nil {
				err = validateReady(cfg, ready)
				if code != http.StatusCreated {
					err = errors.New("invalid_control_response")
				}
				if err == nil {
					sessionURL = cfg.ControlURL + "/sessions/" + url.PathEscape(ready.SessionID)
				}
			}
			if err == nil {
				status.SetControlConnected(true)
				backoff = 0
				continue
			}
		} else {
			var report reportMessage
			code, delay, err = controlRequest(ctx, client, provider, http.MethodPost, sessionURL+"/poll", struct{}{}, &report)
			if code == http.StatusConflict {
				sessionURL = ""
			}
			if err == nil && code != http.StatusOK && code != http.StatusNoContent {
				err = errors.New("invalid_control_response")
			}
			if err == nil {
				status.SetControlConnected(true)
				backoff = 0
				if code == http.StatusNoContent {
					if !waitForControl(ctx, credentialChanged, controlPollInterval) {
						return
					}
					continue
				}
				if report.Type != "report" || !validOpaqueID(report.DeliveryID) || !validOpaqueID(report.Token) || !report.ClaimExpiresAt.After(time.Now()) {
					err = errors.New("invalid_control_response")
				} else {
					if ctx.Err() != nil || !work.Start() {
						return
					}
					func() {
						defer work.Done()
						deadline := report.ClaimExpiresAt
						if maximum := time.Now().Add(time.Minute); deadline.After(maximum) {
							deadline = maximum
						}
						attemptCtx, cancel := context.WithDeadline(workCtx, deadline)
						defer cancel()
						result := deliverReport(attemptCtx, cfg, report)
						submitReportResult(attemptCtx, client, provider, sessionURL, report.DeliveryID, result, credentialChanged, status)
					}()
					continue
				}
			}
		}
		controlFailure(status, code)
		backoff = nextReconnectBackoff(backoff, false, 0)
		if delay <= 0 {
			delay = jitterReconnectDelay(backoff)
		}
		if !waitForControl(ctx, credentialChanged, delay) {
			return
		}
	}
}

func submitReportResult(ctx context.Context, client *http.Client, provider *credentialProvider, sessionURL, deliveryID string, result reportResult, changed <-chan struct{}, status *runtimeStatusManager) {
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		var confirmation struct {
			OK bool `json:"ok"`
		}
		code, delay, err := controlRequest(ctx, client, provider, http.MethodPost, sessionURL+"/report-deliveries/"+url.PathEscape(deliveryID)+"/result", result, &confirmation)
		if err == nil && code == http.StatusOK && confirmation.OK {
			status.SetControlConnected(true)
			return
		}
		if code == http.StatusConflict {
			return
		} // A newer cloud claim owns recovery.
		controlFailure(status, code)
		if code >= 400 && code < 500 && code != 408 && code != 429 && code != 401 && code != 403 {
			return
		}
		backoff = nextReconnectBackoff(backoff, false, 0)
		if delay <= 0 {
			delay = jitterReconnectDelay(backoff)
		}
		if !waitForControl(ctx, changed, delay) {
			return
		}
	}
}
