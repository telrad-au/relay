package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type managedHostedConfig struct {
	ManagementURL            string `json:"managementUrl"`
	ManagementCredentialPath string `json:"managementCredentialPath"`
	StateDirectory           string `json:"stateDirectory"`
}

type hostedLimits struct {
	MaxConnections       int `json:"maxConnections"`
	MaxDicomConnections  int `json:"maxDicomConnections"`
	MaxHL7Connections    int `json:"maxHl7Connections"`
	MaxConcurrentReports int `json:"maxConcurrentReports,omitempty"`
}

type hostedSnapshotEntry struct {
	BindingID         string    `json:"bindingId"`
	Generation        int       `json:"generation"`
	State             string    `json:"state"`
	ConnectorID       string    `json:"connectorId"`
	IPsecConnectionID string    `json:"ipsecConnectionId,omitempty"`
	IngressIdentityIP string    `json:"ingressIdentityIp,omitempty"`
	VPNGeneration     int       `json:"vpnGeneration,omitempty"`
	UsableUntil       time.Time `json:"usableUntil,omitempty"`
	IngestMode        string    `json:"ingestMode,omitempty"`
	Endpoints         struct {
		ControlURL string `json:"controlUrl"`
		DicomURL   string `json:"dicomUrl"`
		HL7URL     string `json:"hl7Url"`
		RenewalURL string `json:"renewalUrl"`
	} `json:"endpoints,omitempty"`
	ReportRoute struct {
		RouteID       string `json:"routeId"`
		EgressLeaseID string `json:"egressLeaseId"`
		ReportHost    string `json:"reportHost"`
		ReportPort    int    `json:"reportPort"`
	} `json:"reportRoute,omitempty"`
	Limits hostedLimits `json:"limits,omitempty"`
}

type hostedSnapshot struct {
	Version        int                   `json:"version"`
	RequestID      string                `json:"requestId"`
	RuntimeID      string                `json:"runtimeId"`
	BootID         string                `json:"bootId"`
	LeaseID        string                `json:"leaseId"`
	Revision       string                `json:"revision"`
	ServerTime     time.Time             `json:"serverTime"`
	LeaseExpiresAt time.Time             `json:"leaseExpiresAt"`
	Limits         hostedLimits          `json:"limits"`
	Bindings       []hostedSnapshotEntry `json:"bindings"`
}

var hostedUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var hostedRevision = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var hostedManagementToken = regexp.MustCompile(`^thr_v1_[A-Za-z0-9_-]{43}$`)
var errHostedRuntimeLeaseHeld = errors.New("hosted runtime lease is held by another boot")

func hostedRandomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

func hostedManagementCredential(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", errors.New("hosted management credential is unavailable")
	}
	data, err := safeReadFile(path, 256)
	if err != nil {
		return "", errors.New("hosted management credential is unavailable")
	}
	token := strings.TrimSpace(string(data))
	if !hostedManagementToken.MatchString(token) {
		return "", errors.New("hosted management credential is invalid")
	}
	return token, nil
}

func validateHostedSnapshot(snapshot hostedSnapshot, requestID, bootID, previousLease, previousRevision string) error {
	if snapshot.Version != 1 || snapshot.RequestID != requestID || snapshot.BootID != bootID ||
		!hostedUUID.MatchString(snapshot.RuntimeID) || !hostedUUID.MatchString(snapshot.LeaseID) ||
		!hostedRevision.MatchString(snapshot.Revision) || snapshot.ServerTime.IsZero() ||
		snapshot.LeaseExpiresAt.Sub(snapshot.ServerTime) <= 0 ||
		snapshot.LeaseExpiresAt.Sub(snapshot.ServerTime) > time.Minute ||
		len(snapshot.Bindings) > 128 || snapshot.Bindings == nil {
		return errors.New("invalid hosted configuration snapshot")
	}
	if previousLease != "" && snapshot.LeaseID != previousLease {
		return errors.New("hosted runtime lease changed without a fresh boot")
	}
	if previousRevision != "" {
		oldRevision, oldOK := new(big.Int).SetString(previousRevision, 10)
		newRevision, newOK := new(big.Int).SetString(snapshot.Revision, 10)
		if !oldOK || !newOK || newRevision.Cmp(oldRevision) < 0 {
			return errors.New("hosted configuration revision moved backward")
		}
	}
	limits := snapshot.Limits
	if limits.MaxConnections < 1 || limits.MaxConnections > 4096 ||
		limits.MaxDicomConnections < 1 || limits.MaxDicomConnections > limits.MaxConnections ||
		limits.MaxHL7Connections < 1 || limits.MaxHL7Connections > limits.MaxConnections ||
		limits.MaxConcurrentReports < 1 || limits.MaxConcurrentReports > 256 {
		return errors.New("invalid hosted process limits")
	}
	sources := make(map[string]struct{})
	bindings := make(map[string]struct{})
	connectors := make(map[string]struct{})
	reserved, dicom, hl7 := 0, 0, 0
	for _, entry := range snapshot.Bindings {
		if !hostedUUID.MatchString(entry.BindingID) || !hostedUUID.MatchString(entry.ConnectorID) ||
			entry.Generation < 1 || (entry.State != "ENABLED" && entry.State != "SUSPENDED" && entry.State != "REVOKED") {
			return errors.New("invalid hosted binding identity")
		}
		if _, exists := bindings[entry.BindingID]; exists {
			return errors.New("duplicate hosted binding")
		}
		if _, exists := connectors[entry.ConnectorID]; exists {
			return errors.New("duplicate hosted connector")
		}
		bindings[entry.BindingID] = struct{}{}
		connectors[entry.ConnectorID] = struct{}{}
		if entry.State != "ENABLED" {
			continue
		}
		ip := net.ParseIP(entry.IngressIdentityIP)
		if ip == nil || ip.To4() == nil || ip.String() != entry.IngressIdentityIP ||
			!hostedUUID.MatchString(entry.IPsecConnectionID) || entry.VPNGeneration < 1 ||
			entry.UsableUntil.Sub(snapshot.ServerTime) <= 0 ||
			entry.UsableUntil.After(snapshot.LeaseExpiresAt) ||
			!hostedUUID.MatchString(entry.ReportRoute.RouteID) ||
			!hostedUUID.MatchString(entry.ReportRoute.EgressLeaseID) ||
			entry.ReportRoute.ReportPort < 1 || entry.ReportRoute.ReportPort > 65535 ||
			entry.IngestMode != "PRODUCTION" && entry.IngestMode != "TEST" {
			return errors.New("invalid enabled hosted binding")
		}
		reportIP := net.ParseIP(entry.ReportRoute.ReportHost)
		if reportIP == nil || reportIP.To4() == nil || reportIP.String() != entry.ReportRoute.ReportHost {
			return errors.New("invalid hosted report route")
		}
		if _, exists := sources[entry.IngressIdentityIP]; exists {
			return errors.New("duplicate hosted ingress identity")
		}
		sources[entry.IngressIdentityIP] = struct{}{}
		for name, endpoint := range map[string]string{
			"controlUrl": entry.Endpoints.ControlURL,
			"dicomUrl":   entry.Endpoints.DicomURL,
			"hl7Url":     entry.Endpoints.HL7URL,
			"renewalUrl": entry.Endpoints.RenewalURL,
		} {
			if err := validateEndpointURL(name, endpoint, "https", ""); err != nil {
				return err
			}
		}
		if entry.Limits.MaxConnections < 1 ||
			entry.Limits.MaxDicomConnections < 1 ||
			entry.Limits.MaxHL7Connections < 1 ||
			entry.Limits.MaxDicomConnections > entry.Limits.MaxConnections ||
			entry.Limits.MaxHL7Connections > entry.Limits.MaxConnections {
			return errors.New("invalid hosted binding limits")
		}
		reserved += entry.Limits.MaxConnections
		dicom += entry.Limits.MaxDicomConnections
		hl7 += entry.Limits.MaxHL7Connections
	}
	if reserved > limits.MaxConnections || dicom > limits.MaxDicomConnections || hl7 > limits.MaxHL7Connections {
		return errors.New("hosted reservations exceed process budget")
	}
	return nil
}

func fetchHostedSnapshot(ctx context.Context, client *http.Client, cfg *managedHostedConfig, token, bootID, leaseID, revision string) (hostedSnapshot, time.Time, error) {
	requestID, err := hostedRandomID()
	if err != nil {
		return hostedSnapshot{}, time.Time{}, err
	}
	var lease any
	if leaseID != "" {
		lease = leaseID
	}
	body, err := json.Marshal(map[string]any{
		"version": 1, "requestId": requestID, "bootId": bootID,
		"leaseId": lease, "knownRevision": revision,
	})
	if err != nil {
		return hostedSnapshot{}, time.Time{}, err
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ManagementURL, bytes.NewReader(body))
	if err != nil {
		return hostedSnapshot{}, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return hostedSnapshot{}, time.Time{}, safeNetworkError(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return hostedSnapshot{}, time.Time{}, errHostedRuntimeLeaseHeld
	}
	if response.StatusCode != http.StatusOK {
		return hostedSnapshot{}, time.Time{}, fmt.Errorf("hosted configuration unavailable (%d)", response.StatusCode)
	}
	var snapshot hostedSnapshot
	if err := decodeBoundedJSON(response.Body, maxCloudResponseBytes, &snapshot); err != nil {
		return hostedSnapshot{}, time.Time{}, errors.New("invalid hosted configuration response")
	}
	if err := validateHostedSnapshot(snapshot, requestID, bootID, leaseID, revision); err != nil {
		return hostedSnapshot{}, time.Time{}, err
	}
	return snapshot, start, nil
}

type hostedHeaderTransport struct {
	base       http.RoundTripper
	bindingID  string
	generation int
	leaseID    string
}

func (transport hostedHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header = req.Header.Clone()
	copy.Header.Set("X-Telrad-Hosted-Binding", transport.bindingID)
	copy.Header.Set("X-Telrad-Hosted-Generation", strconv.Itoa(transport.generation))
	copy.Header.Set("X-Telrad-Hosted-Lease", transport.leaseID)
	return transport.base.RoundTrip(copy)
}

type managedHostedRegistry struct {
	mu       sync.RWMutex
	bindings map[string]*hostedBinding
}

func (registry *managedHostedRegistry) lookup(peer net.Addr) *hostedBinding {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return hostedBindingForPeer(peer, registry.bindings)
}

func (registry *managedHostedRegistry) stopAll() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for source, binding := range registry.bindings {
		delete(registry.bindings, source)
		binding.stop()
	}
}

func hostedDeadline(start time.Time, serverTime, usableUntil time.Time) time.Time {
	return start.Add(usableUntil.Sub(serverTime))
}

func hostedBootstrapURL(configurationURL, bindingID string) (string, error) {
	u, err := url.Parse(configurationURL)
	if err != nil {
		return "", err
	}
	u.Path = "/internal/relay/hosted/bindings/" + bindingID + "/credentials"
	return u.String(), nil
}

// A pending operation is fsynced before requesting credential material, so a
// lost HTTP response can be replayed without minting a second family.
type hostedBootstrapOperation struct {
	OperationID string `json:"operationId"`
}

func newHostedOperationID() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func readHostedOperation(path string) (string, error) {
	data, err := safeReadFile(path, 256)
	if err != nil {
		return "", err
	}
	var operation hostedBootstrapOperation
	if err := json.Unmarshal(data, &operation); err != nil ||
		!credentialOperationPattern.MatchString(operation.OperationID) {
		return "", errors.New("invalid hosted bootstrap operation")
	}
	return operation.OperationID, nil
}

func hostedCredentialPath(cfg *managedHostedConfig, bindingID string) string {
	return filepath.Join(cfg.StateDirectory, "bindings", bindingID, "relay-credential.json")
}

func hostedConfigPath(cfg *managedHostedConfig, bindingID string, generation int) string {
	return filepath.Join(cfg.StateDirectory, "bindings", bindingID, fmt.Sprintf("config-generation-%d.json", generation))
}
