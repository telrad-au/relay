package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	runtimeStatusInterval = 30 * time.Second
	runtimeStatusMaxAge   = 90 * time.Second
)

type relayRuntimeStatus struct {
	State                   string    `json:"state"`
	UpdatedAt               time.Time `json:"updatedAt"`
	Version                 string    `json:"version"`
	ProcessID               int       `json:"processId"`
	IngestReady             bool      `json:"ingestReady"`
	ControlConnected        bool      `json:"controlConnected"`
	ReportReturnAvailable   bool      `json:"reportReturnAvailable"`
	AuthenticationAttention bool      `json:"authenticationAttention"`
	LastErrorCode           string    `json:"lastErrorCode,omitempty"`
}

type runtimeStatusManager struct {
	mu         sync.Mutex
	configPath string
	status     relayRuntimeStatus
	authCloud  bool
	authFile   bool
}

func newRuntimeStatus(configPath string) *runtimeStatusManager {
	return &runtimeStatusManager{configPath: configPath, status: relayRuntimeStatus{State: "starting", Version: version, ProcessID: os.Getpid()}}
}

func (manager *runtimeStatusManager) SetIngestReady(value bool) {
	manager.mu.Lock()
	if manager.status.IngestReady == value {
		manager.mu.Unlock()
		return
	}
	manager.status.IngestReady = value
	manager.refreshState()
	manager.mu.Unlock()
	_ = manager.write()
}

func (manager *runtimeStatusManager) SetControlConnected(value bool) {
	manager.mu.Lock()
	if manager.status.ControlConnected == value && manager.status.ReportReturnAvailable == value {
		manager.mu.Unlock()
		return
	}
	manager.status.ControlConnected = value
	manager.status.ReportReturnAvailable = value
	manager.refreshState()
	manager.mu.Unlock()
	_ = manager.write()
}

func (manager *runtimeStatusManager) SetAuthenticationAttention(value bool) {
	manager.mu.Lock()
	if manager.authCloud == value {
		manager.mu.Unlock()
		return
	}
	manager.authCloud = value
	manager.applyAuthenticationAttention()
	manager.mu.Unlock()
	_ = manager.write()
}

func (manager *runtimeStatusManager) SetCredentialFileAttention(value bool) {
	manager.mu.Lock()
	if manager.authFile == value {
		manager.mu.Unlock()
		return
	}
	manager.authFile = value
	manager.applyAuthenticationAttention()
	manager.mu.Unlock()
	_ = manager.write()
}

func (manager *runtimeStatusManager) CredentialAdopted() {
	manager.mu.Lock()
	manager.authCloud = false
	manager.authFile = false
	manager.applyAuthenticationAttention()
	manager.mu.Unlock()
	_ = manager.write()
}

func (manager *runtimeStatusManager) applyAuthenticationAttention() {
	manager.status.AuthenticationAttention = manager.authCloud || manager.authFile
	if manager.status.AuthenticationAttention {
		manager.status.LastErrorCode = "authentication_required"
	} else if manager.status.LastErrorCode == "authentication_required" {
		manager.status.LastErrorCode = ""
	}
	manager.refreshState()
}

func (manager *runtimeStatusManager) refreshState() {
	switch {
	case manager.status.AuthenticationAttention:
		manager.status.State = "attention"
	case manager.status.IngestReady:
		manager.status.State = "ready"
	default:
		manager.status.State = "starting"
	}
}

func (manager *runtimeStatusManager) write() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.status.UpdatedAt = time.Now().UTC()
	return atomicWriteJSON(runtimeStatusPath(manager.configPath), manager.status)
}

func maintainRuntimeStatus(ctx context.Context, manager *runtimeStatusManager, errCh chan<- error) {
	if err := manager.write(); err != nil {
		errCh <- err
		return
	}
	ticker := time.NewTicker(runtimeStatusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := manager.write(); err != nil {
				errCh <- err
				return
			}
		}
	}
}

// Compatibility wrapper used by updater and older installer health checks.
func writeRuntimeStatus(configPath, state string, err error) error {
	status := relayRuntimeStatus{State: state, UpdatedAt: time.Now().UTC(), Version: version, ProcessID: os.Getpid(), IngestReady: state == "ready"}
	if err != nil {
		status.LastErrorCode = "runtime_error"
	}
	return atomicWriteJSON(runtimeStatusPath(configPath), status)
}

func maintainReadyStatus(ctx context.Context, configPath string) error {
	manager := newRuntimeStatus(configPath)
	manager.SetIngestReady(true)
	errCh := make(chan error, 1)
	go maintainRuntimeStatus(ctx, manager, errCh)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func checkRuntimeReady(configPath string, now time.Time) error {
	status, err := readRuntimeStatus(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("relay runtime status is unavailable")
	}
	if err != nil {
		return err
	}
	if status.State != "ready" || !status.IngestReady || status.AuthenticationAttention {
		return fmt.Errorf("relay state is %s", status.State)
	}
	if status.ProcessID <= 0 || status.UpdatedAt.IsZero() {
		return errors.New("relay runtime status is incomplete")
	}
	age := now.Sub(status.UpdatedAt)
	if age < -runtimeStatusInterval || age > runtimeStatusMaxAge {
		return fmt.Errorf("relay ready status is stale by %s", age.Round(time.Second))
	}
	return nil
}

func printRuntimeStatus(status relayRuntimeStatus) {
	fmt.Printf("ingest ready: %t\ncontrol connected: %t\nreport return available: %t\nauthentication attention: %t\n", status.IngestReady, status.ControlConnected, status.ReportReturnAvailable, status.AuthenticationAttention)
}

func readRuntimeStatus(configPath string) (relayRuntimeStatus, error) {
	data, err := os.ReadFile(runtimeStatusPath(configPath))
	if err != nil {
		return relayRuntimeStatus{}, err
	}
	var status relayRuntimeStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return relayRuntimeStatus{}, fmt.Errorf("decode relay runtime status: %w", err)
	}
	return status, nil
}

func runtimeStatusPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "runtime-status.json")
}
