package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// The static manifest is for disposable VPN qualification. The managed
// configuration lease and backend acceptance gate are required before release.
type hostedManifest struct {
	SchemaVersion       int                   `json:"schemaVersion"`
	ListenAddress       string                `json:"listenAddress"`
	DicomPort           int                   `json:"dicomPort"`
	HL7Port             int                   `json:"hl7Port"`
	MaxConnections      int                   `json:"maxConnections"`
	MaxDicomConnections int                   `json:"maxDicomConnections"`
	MaxHL7Connections   int                   `json:"maxHl7Connections"`
	Bindings            []hostedManifestEntry `json:"bindings"`
}

type hostedManifestEntry struct {
	SourceIP   string `json:"sourceIp"`
	ConfigPath string `json:"configPath"`
}

type hostedBinding struct {
	cfg      *config
	provider *credentialProvider
	clients  protocolClients
	status   *runtimeStatusManager
	limits   *connectionLimiter
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	sockets  map[net.Conn]struct{}
}

func loadHostedManifest(path string) (*hostedManifest, map[string]*hostedBinding, error) {
	data, err := safeReadFile(path, maxCloudResponseBytes)
	if err != nil {
		return nil, nil, err
	}
	var manifest hostedManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, nil, fmt.Errorf("decode hosted manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("hosted manifest has multiple JSON values")
	}
	if manifest.SchemaVersion != 1 || net.ParseIP(manifest.ListenAddress) == nil || manifest.DicomPort < 1 || manifest.DicomPort > 65535 || manifest.HL7Port < 1 || manifest.HL7Port > 65535 || manifest.DicomPort == manifest.HL7Port {
		return nil, nil, errors.New("invalid hosted listener configuration")
	}
	if manifest.MaxConnections < 1 || manifest.MaxConnections > 4096 || manifest.MaxDicomConnections < 1 || manifest.MaxHL7Connections < 1 || manifest.MaxDicomConnections > manifest.MaxConnections || manifest.MaxHL7Connections > manifest.MaxConnections || len(manifest.Bindings) < 1 || len(manifest.Bindings) > 128 {
		return nil, nil, errors.New("invalid hosted connection limits or bindings")
	}
	bindings := make(map[string]*hostedBinding, len(manifest.Bindings))
	ids := make(map[string]struct{}, len(manifest.Bindings))
	paths := make(map[string]struct{}, len(manifest.Bindings))
	credentialPaths := make(map[string]struct{}, len(manifest.Bindings))
	for _, entry := range manifest.Bindings {
		ip := net.ParseIP(entry.SourceIP)
		if ip == nil || ip.To4() == nil || entry.SourceIP != ip.String() || entry.ConfigPath == "" {
			return nil, nil, errors.New("hosted binding source or configuration path is invalid")
		}
		if _, exists := bindings[entry.SourceIP]; exists {
			return nil, nil, errors.New("duplicate hosted source")
		}
		childPath := absolute(filepath.Dir(path), entry.ConfigPath)
		if _, exists := paths[childPath]; exists {
			return nil, nil, errors.New("duplicate hosted configuration")
		}
		paths[childPath] = struct{}{}
		cfg, err := loadConfigMode(childPath, false)
		if err != nil {
			return nil, nil, fmt.Errorf("load hosted child: %w", err)
		}
		if err := validateConfig(cfg, "run"); err != nil {
			return nil, nil, fmt.Errorf("validate hosted child: %w", err)
		}
		if cfg.Retrieval != nil || cfg.DisableDICOMListener {
			return nil, nil, errors.New("hosted child must use push DICOM and cannot enable PACS retrieval")
		}
		if reportIP := net.ParseIP(cfg.ReportHost); reportIP == nil || reportIP.To4() == nil || reportIP.String() != cfg.ReportHost {
			return nil, nil, errors.New("hosted report destination must be a canonical IPv4 address")
		}
		if _, exists := credentialPaths[cfg.CredentialPath]; exists {
			return nil, nil, errors.New("hosted children share a credential directory")
		}
		credentialPaths[cfg.CredentialPath] = struct{}{}
		for previous := range credentialPaths {
			if previous != cfg.CredentialPath && filepath.Dir(previous) == filepath.Dir(cfg.CredentialPath) {
				return nil, nil, errors.New("hosted children share a signing-key directory")
			}
		}
		if _, exists := ids[cfg.RelayID]; exists {
			return nil, nil, errors.New("duplicate hosted connector")
		}
		ids[cfg.RelayID] = struct{}{}
		provider, err := newCredentialProvider(cfg.CredentialPath, time.Now())
		if err != nil {
			return nil, nil, errors.New("hosted child credential is invalid")
		}
		bindings[entry.SourceIP] = &hostedBinding{cfg: cfg, provider: provider, clients: clientFactory(cfg), status: newRuntimeStatus(childPath), limits: newConnectionLimiter(cfg), sockets: make(map[net.Conn]struct{})}
	}
	return &manifest, bindings, nil
}

func (binding *hostedBinding) register(conn net.Conn) bool {
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.closed {
		return false
	}
	binding.sockets[conn] = struct{}{}
	return true
}

func (binding *hostedBinding) unregister(conn net.Conn) {
	binding.mu.Lock()
	delete(binding.sockets, conn)
	binding.mu.Unlock()
}

func (binding *hostedBinding) stop() {
	binding.mu.Lock()
	binding.closed = true
	binding.cancel()
	for conn := range binding.sockets {
		_ = conn.Close()
	}
	binding.mu.Unlock()
	binding.status.SetIngestReady(false)
}

func runHosted(ctx context.Context, path string) error {
	manifest, bindings, err := loadHostedManifest(path)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if err := ensureReportSigningKey(binding.cfg); err != nil {
			return err
		}
	}
	dicomListener, err := net.Listen("tcp", net.JoinHostPort(manifest.ListenAddress, strconv.Itoa(manifest.DicomPort)))
	if err != nil {
		return fmt.Errorf("listen for hosted DICOM: %w", err)
	}
	defer dicomListener.Close()
	hl7Listener, err := net.Listen("tcp", net.JoinHostPort(manifest.ListenAddress, strconv.Itoa(manifest.HL7Port)))
	if err != nil {
		return fmt.Errorf("listen for hosted HL7: %w", err)
	}
	defer hl7Listener.Close()
	work := newWorkDrainer()
	globalLimits := newConnectionLimiter(&config{MaxConnections: manifest.MaxConnections, MaxDicomConnections: manifest.MaxDicomConnections, MaxHL7Connections: manifest.MaxHL7Connections})
	errCh := make(chan error, len(bindings)+2)
	for _, binding := range bindings {
		binding.ctx, binding.cancel = context.WithCancel(ctx)
		binding.status.SetIngestReady(true)
		credentialChanged := make(chan struct{}, 1)
		lifecycleChanged := make(chan struct{}, 1)
		go watchCredentialFile(binding.ctx, binding.provider, binding.status, credentialChanged, lifecycleChanged)
		go maintainCredentialLifecycle(binding.ctx, binding.cfg, binding.clients.secure, binding.provider, credentialChanged, lifecycleChanged, binding.status)
		go superviseControl(binding.ctx, binding.ctx, binding.cfg, binding.clients.secure, binding.provider, credentialChanged, work, binding.status)
		go maintainRuntimeStatus(binding.ctx, binding.status, errCh)
	}
	var listeners sync.WaitGroup
	for _, item := range []struct {
		protocol string
		listener net.Listener
	}{{"dicom", dicomListener}, {"hl7", hl7Listener}} {
		listeners.Add(1)
		go func(protocol string, listener net.Listener) {
			defer listeners.Done()
			acceptHostedConnections(ctx, protocol, listener, bindings, work, globalLimits, errCh)
		}(item.protocol, item.listener)
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}
	_ = dicomListener.Close()
	_ = hl7Listener.Close()
	listeners.Wait()
	for _, binding := range bindings {
		binding.stop()
	}
	if err := drainRelayWork(work); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func acceptHostedConnections(ctx context.Context, protocol string, listener net.Listener, bindings map[string]*hostedBinding, work *workDrainer, global *connectionLimiter, errCh chan<- error) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				errCh <- err
			}
			return
		}
		binding := hostedBindingForPeer(conn.RemoteAddr(), bindings)
		if binding == nil || !global.Acquire(protocol) {
			_ = conn.Close()
			continue
		}
		if !binding.limits.Acquire(protocol) {
			global.Release(protocol)
			_ = conn.Close()
			continue
		}
		if !binding.register(conn) {
			binding.limits.Release(protocol)
			global.Release(protocol)
			_ = conn.Close()
			continue
		}
		if !work.Start() {
			binding.unregister(conn)
			binding.limits.Release(protocol)
			global.Release(protocol)
			_ = conn.Close()
			continue
		}
		go func() {
			defer work.Done()
			defer global.Release(protocol)
			defer binding.limits.Release(protocol)
			defer binding.unregister(conn)
			defer conn.Close()
			if protocol == "dicom" {
				serveDICOM(binding.ctx, conn, binding.cfg, binding.clients.secure, binding.provider, binding.status)
			} else {
				serveHL7(binding.ctx, conn, binding.cfg, binding.clients.secure, binding.provider, binding.status)
			}
		}()
	}
}

func hostedBindingForPeer(address net.Addr, bindings map[string]*hostedBinding) *hostedBinding {
	peer, ok := address.(*net.TCPAddr)
	if !ok || peer.IP.To4() == nil {
		return nil
	}
	return bindings[peer.IP.String()]
}
