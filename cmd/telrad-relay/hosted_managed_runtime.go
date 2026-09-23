package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

func bootstrapHostedCredential(
	ctx context.Context,
	client *http.Client,
	management *managedHostedConfig,
	token, bindingID, leaseID string,
	generation int,
) (string, error) {
	credentialPath := hostedCredentialPath(management, bindingID)
	if _, err := os.Lstat(credentialPath); err == nil {
		if _, err := readCredentialFile(credentialPath, time.Now()); err != nil {
			return "", errors.New("hosted binding credential is invalid")
		}
		return credentialPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("hosted binding credential is unavailable")
	}
	if err := secureCredentialDirectory(filepath.Dir(credentialPath)); err != nil {
		return "", err
	}
	operationPath := filepath.Join(filepath.Dir(credentialPath), "bootstrap-operation.json")
	operationID, err := readHostedOperation(operationPath)
	if errors.Is(err, os.ErrNotExist) {
		operationID, err = newHostedOperationID()
		if err != nil {
			return "", err
		}
		if err := atomicWriteJSON(operationPath, hostedBootstrapOperation{OperationID: operationID}); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	endpoint, err := hostedBootstrapURL(management.ManagementURL, bindingID)
	if err != nil {
		return "", err
	}
	requestBody, err := json.Marshal(map[string]any{
		"version": 1, "leaseId": leaseID, "generation": generation, "operationId": operationID,
	})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", safeNetworkError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hosted credential bootstrap unavailable (%d)", response.StatusCode)
	}
	var material credentialFile
	if err := decodeBoundedJSON(response.Body, maxCloudResponseBytes, &material); err != nil {
		return "", errors.New("invalid hosted credential response")
	}
	material.SchemaVersion = credentialLifecycleSchemaVersion
	material.AccessObtainedAt = time.Now()
	if err := material.validate(material.AccessObtainedAt); err != nil {
		return "", errors.New("invalid hosted credential material")
	}
	if err := atomicWriteJSON(credentialPath, material); err != nil {
		return "", err
	}
	if err := os.Remove(operationPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return credentialPath, nil
}

func hostedEntryFingerprint(entry hostedSnapshotEntry, leaseID string) string {
	entry.UsableUntil = time.Time{}
	data, _ := json.Marshal(struct {
		Entry hostedSnapshotEntry `json:"entry"`
		Lease string              `json:"lease"`
	}{entry, leaseID})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func buildManagedBinding(
	ctx context.Context,
	root *config,
	entry hostedSnapshotEntry,
	managementToken, leaseID string,
	client *http.Client,
	deadline time.Time,
	work *workDrainer,
	errCh chan<- error,
) (*hostedBinding, error) {
	path := hostedConfigPath(root.HostedRuntime, entry.BindingID, entry.Generation)
	credentialPath, err := bootstrapHostedCredential(ctx, client, root.HostedRuntime, managementToken, entry.BindingID, leaseID, entry.Generation)
	if err != nil {
		return nil, err
	}
	child := *root
	child.HostedRuntime = nil
	child.Bindings = nil
	child.Retrieval = nil
	child.RelayID = entry.ConnectorID
	control, err := url.Parse(entry.Endpoints.ControlURL)
	if err != nil {
		return nil, err
	}
	child.PairingURL = control.Scheme + "://" + control.Host + "/v1/relay/pairing-enrollments"
	child.ControlURL = entry.Endpoints.ControlURL
	child.DicomURL = entry.Endpoints.DicomURL
	child.HL7URL = entry.Endpoints.HL7URL
	child.CredentialPath = credentialPath
	child.credentialPathConfigured = credentialPath
	child.ReportHost = entry.ReportRoute.ReportHost
	child.ReportPort = entry.ReportRoute.ReportPort
	child.MaxConnections = entry.Limits.MaxConnections
	child.MaxDicomConnections = entry.Limits.MaxDicomConnections
	child.MaxHL7Connections = entry.Limits.MaxHL7Connections
	child.configPath = path
	if err := validateConfig(&child, "run"); err != nil {
		return nil, err
	}
	if err := atomicWriteJSON(path, &child); err != nil {
		return nil, err
	}
	if err := ensureReportSigningKey(&child); err != nil {
		return nil, err
	}
	provider, err := newCredentialProvider(credentialPath, time.Now())
	if err != nil {
		return nil, errors.New("hosted binding credential is unavailable")
	}
	clients := clientFactory(&child)
	secure := *clients.secure
	secure.Transport = hostedHeaderTransport{
		base: clients.secure.Transport, bindingID: entry.BindingID,
		generation: entry.Generation, leaseID: leaseID,
	}
	clients.secure = &secure
	binding := &hostedBinding{
		cfg: &child, provider: provider, clients: clients,
		status: newRuntimeStatus(path), limits: newConnectionLimiter(&child),
		sockets:    make(map[net.Conn]struct{}),
		validUntil: deadline, fingerprint: hostedEntryFingerprint(entry, leaseID),
	}
	binding.ctx, binding.cancel = context.WithCancel(ctx)
	binding.status.SetIngestReady(true)
	credentialChanged := make(chan struct{}, 1)
	lifecycleChanged := make(chan struct{}, 1)
	go watchCredentialFile(binding.ctx, binding.provider, binding.status, credentialChanged, lifecycleChanged)
	go maintainCredentialLifecycle(binding.ctx, binding.cfg, binding.clients.secure, binding.provider, credentialChanged, lifecycleChanged, binding.status)
	go superviseControl(binding.ctx, binding.ctx, binding.cfg, binding.clients.secure, binding.provider, credentialChanged, work, binding.status)
	go maintainRuntimeStatus(binding.ctx, binding.status, errCh)
	return binding, nil
}

func applyManagedSnapshot(
	ctx context.Context,
	registry *managedHostedRegistry,
	root *config,
	snapshot hostedSnapshot,
	requestStart time.Time,
	managementToken string,
	client *http.Client,
	work *workDrainer,
	errCh chan<- error,
) error {
	prepared := make(map[string]*hostedBinding)
	for _, entry := range snapshot.Bindings {
		if entry.State != "ENABLED" {
			continue
		}
		deadline := hostedDeadline(requestStart, snapshot.ServerTime, entry.UsableUntil)
		if !time.Now().Before(deadline) {
			continue
		}
		fingerprint := hostedEntryFingerprint(entry, snapshot.LeaseID)
		registry.mu.RLock()
		current := registry.bindings[entry.IngressIdentityIP]
		same := current != nil && current.fingerprint == fingerprint
		registry.mu.RUnlock()
		if same {
			continue
		}
		binding, err := buildManagedBinding(ctx, root, entry, managementToken, snapshot.LeaseID, client, deadline, work, errCh)
		if err != nil {
			for _, ready := range prepared {
				ready.stop()
			}
			return fmt.Errorf("prepare hosted binding: %w", err)
		}
		prepared[entry.IngressIdentityIP] = binding
	}
	next := make(map[string]*hostedBinding)
	registry.mu.Lock()
	for _, entry := range snapshot.Bindings {
		if entry.State != "ENABLED" {
			continue
		}
		deadline := hostedDeadline(requestStart, snapshot.ServerTime, entry.UsableUntil)
		if !time.Now().Before(deadline) {
			continue
		}
		if ready := prepared[entry.IngressIdentityIP]; ready != nil {
			next[entry.IngressIdentityIP] = ready
		} else if current := registry.bindings[entry.IngressIdentityIP]; current != nil {
			current.setDeadline(deadline)
			next[entry.IngressIdentityIP] = current
		}
	}
	old := registry.bindings
	registry.bindings = next
	registry.mu.Unlock()
	for source, binding := range old {
		if next[source] != binding {
			binding.stop()
		}
	}
	return nil
}

func pruneManagedBindings(registry *managedHostedRegistry, now time.Time) {
	registry.mu.Lock()
	var expired []*hostedBinding
	for source, binding := range registry.bindings {
		if binding.expired(now) {
			delete(registry.bindings, source)
			expired = append(expired, binding)
		}
	}
	registry.mu.Unlock()
	for _, binding := range expired {
		binding.stop()
	}
}

func runManagedHosted(ctx context.Context, root *config) error {
	management := root.HostedRuntime
	if management == nil {
		return errors.New("hosted runtime configuration is required")
	}
	if err := secureCredentialDirectory(management.StateDirectory); err != nil {
		return err
	}
	lock, err := acquireCredentialOperationFileLock(filepath.Join(management.StateDirectory, "runtime"))
	if err != nil {
		return errors.New("hosted state directory is already in use")
	}
	defer lock.Close()
	token, err := hostedManagementCredential(management.ManagementCredentialPath)
	if err != nil {
		return err
	}
	bootID, err := hostedRandomID()
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var snapshot hostedSnapshot
	var start time.Time
	for {
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		snapshot, start, err = fetchHostedSnapshot(requestCtx, client, management, token, bootID, "", "0")
		cancel()
		if !errors.Is(err, errHostedRuntimeLeaseHeld) {
			break
		}
		slog.Info("waiting for previous hosted runtime lease")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if err != nil {
		return err
	}
	if snapshot.Limits.MaxConnections > root.MaxConnections ||
		snapshot.Limits.MaxDicomConnections > root.MaxDicomConnections ||
		snapshot.Limits.MaxHL7Connections > root.MaxHL7Connections {
		return errors.New("hosted snapshot exceeds configured listener budget")
	}
	dicomListener, err := net.Listen("tcp", net.JoinHostPort(root.ListenAddress, strconv.Itoa(root.DicomPort)))
	if err != nil {
		return err
	}
	defer dicomListener.Close()
	hl7Listener, err := net.Listen("tcp", net.JoinHostPort(root.ListenAddress, strconv.Itoa(root.HL7Port)))
	if err != nil {
		return err
	}
	defer hl7Listener.Close()
	registry := &managedHostedRegistry{bindings: make(map[string]*hostedBinding)}
	defer registry.stopAll()
	work := newWorkDrainer()
	errCh := make(chan error, 128)
	if err := applyManagedSnapshot(ctx, registry, root, snapshot, start, token, client, work, errCh); err != nil {
		return err
	}
	leaseID, revision := snapshot.LeaseID, snapshot.Revision
	leaseDeadline := hostedDeadline(start, snapshot.ServerTime, snapshot.LeaseExpiresAt)
	limits := newConnectionLimiter(root)
	var listeners sync.WaitGroup
	for _, item := range []struct {
		protocol string
		listener net.Listener
	}{{"dicom", dicomListener}, {"hl7", hl7Listener}} {
		listeners.Add(1)
		go func(protocol string, listener net.Listener) {
			defer listeners.Done()
			acceptHostedWithLookup(ctx, protocol, listener, registry.lookup, work, limits, errCh)
		}(item.protocol, item.listener)
	}
	refresh := time.NewTicker(15 * time.Second)
	prune := time.NewTicker(time.Second)
	defer refresh.Stop()
	defer prune.Stop()
	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case runErr = <-errCh:
			break loop
		case <-prune.C:
			pruneManagedBindings(registry, time.Now())
			if !time.Now().Before(leaseDeadline) {
				registry.stopAll()
				leaseID = ""
			}
		case <-refresh.C:
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			fresh, started, fetchErr := fetchHostedSnapshot(requestCtx, client, management, token, bootID, leaseID, revision)
			cancel()
			if fetchErr != nil {
				slog.Warn("hosted configuration refresh failed", "error", fetchErr.Error())
				continue
			}
			if fresh.RuntimeID != snapshot.RuntimeID ||
				fresh.Limits.MaxConnections > root.MaxConnections ||
				fresh.Limits.MaxDicomConnections > root.MaxDicomConnections ||
				fresh.Limits.MaxHL7Connections > root.MaxHL7Connections {
				slog.Warn("hosted configuration identity or budget changed")
				continue
			}
			if err := applyManagedSnapshot(ctx, registry, root, fresh, started, token, client, work, errCh); err != nil {
				slog.Warn("hosted configuration cannot be applied", "error", err.Error())
				continue
			}
			snapshot, leaseID, revision = fresh, fresh.LeaseID, fresh.Revision
			leaseDeadline = hostedDeadline(started, fresh.ServerTime, fresh.LeaseExpiresAt)
		}
	}
	_ = dicomListener.Close()
	_ = hl7Listener.Close()
	listeners.Wait()
	registry.stopAll()
	if err := drainRelayWork(work); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}
