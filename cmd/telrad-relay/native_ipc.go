//go:build !relay_container

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const managementProtocol = 1

// SCM/systemd accepting a start request is distinct from a usable local service.
func waitForManagement(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		conn, err := dialManagement(ctx, false)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("Relay management did not start")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type managementRequest struct {
	Version int    `json:"version"`
	Action  string `json:"action"`
}
type managementResponse struct {
	Version int    `json:"version"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

func managementMutation(action string) bool {
	return action == "enroll" || action == "rotate-credential"
}
func managementAction(action string) bool {
	switch action {
	case "status", "ready", "doctor", "enroll", "rotate-credential":
		return true
	}
	return false
}

// Text is display-only. No server response can request a file operation, launch
// a browser, supply command arguments, or provide an update trust decision.
func managementText(s string) string {
	if len(s) > maxCloudResponseBytes {
		s = s[:maxCloudResponseBytes]
	}
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' || r == 127 {
			return -1
		}
		return r
	}, s)
}

type managementWriter struct{ encoder *json.Encoder }

func (w managementWriter) Write(p []byte) (int, error) {
	if len(p) > maxCloudResponseBytes {
		return 0, errors.New("management output exceeds limit")
	}
	err := w.encoder.Encode(managementResponse{Version: managementProtocol, Message: managementText(string(p))})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func callManagement(ctx context.Context, action string, output io.Writer) error {
	if !managementAction(action) {
		return errors.New("unknown management action")
	}
	conn, err := dialManagement(ctx, managementMutation(action))
	if err != nil {
		return errors.New("Relay management is unavailable; run 'telrad start' before this operation")
	}
	defer conn.Close()
	cancelClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer cancelClose()
	_ = conn.SetDeadline(time.Now().Add(deviceAuthorizationTimeout + time.Minute))
	if err := json.NewEncoder(conn).Encode(managementRequest{Version: managementProtocol, Action: action}); err != nil {
		return err
	}
	scanner := bufio.NewScanner(io.LimitReader(conn, 1024*1024))
	scanner.Buffer(make([]byte, 4096), maxCloudResponseBytes+1024)
	for scanner.Scan() {
		var r managementResponse
		if err := strictJSON(scanner.Bytes(), &r); err != nil || r.Version != managementProtocol {
			return errors.New("invalid Relay management response")
		}
		if r.Message != "" {
			if _, err := io.WriteString(output, managementText(r.Message)); err != nil {
				return err
			}
		}
		if r.Done {
			if r.Error != "" {
				return errors.New(managementText(r.Error))
			}
			return nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("Relay management disconnected before completion")
}

func strictJSON(data []byte, target any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

type clinicalTransition struct {
	stop   bool
	result chan error
}
type managementServer struct {
	path        string
	mutation    sync.Mutex
	transitions chan clinicalTransition
	lifetime    context.Context
}

func (m *managementServer) transition(ctx context.Context, stop bool) error {
	request := clinicalTransition{stop: stop, result: make(chan error, 1)}
	select {
	case m.transitions <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Once accepted, the service owns completion. A client disconnect must not
	// abandon an in-flight drain before its caller can arrange the resume.
	lifetime := m.lifetime
	if lifetime == nil {
		lifetime = context.WithoutCancel(ctx)
	}
	select {
	case err := <-request.result:
		return err
	case <-lifetime.Done():
		return lifetime.Err()
	}
}

func (m *managementServer) resumeAfterPairing(ctx context.Context) error {
	lifetime := m.lifetime
	if lifetime == nil {
		lifetime = context.WithoutCancel(ctx)
	}
	retryCtx, cancel := context.WithTimeout(lifetime, serviceDrainTimeout+10*time.Second)
	defer cancel()
	return m.transition(retryCtx, false)
}

func (m *managementServer) serve(ctx context.Context, conn net.Conn, administrator bool) {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, deviceAuthorizationTimeout+time.Minute)
	defer cancel()
	cancelClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer cancelClose()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024), 4096)
	if !scanner.Scan() {
		return
	}
	var request managementRequest
	enc := json.NewEncoder(conn)
	finish := func(err error) {
		r := managementResponse{Version: managementProtocol, Done: true}
		if err != nil {
			r.Error = managementText(err.Error())
		}
		_ = enc.Encode(r)
	}
	if err := strictJSON(scanner.Bytes(), &request); err != nil || request.Version != managementProtocol || !managementAction(request.Action) {
		finish(errors.New("invalid management request"))
		return
	}
	if managementMutation(request.Action) && !administrator {
		finish(errors.New("administrator authorization is required"))
		return
	}
	_ = conn.SetDeadline(time.Now().Add(deviceAuthorizationTimeout + time.Minute))
	// After the single request, disconnect cancels outstanding authorization.
	go func() { var b [1]byte; _, _ = conn.Read(b[:]); cancel() }()
	out := managementWriter{enc}
	if managementMutation(request.Action) {
		if !m.mutation.TryLock() {
			finish(errors.New("another credential operation is in progress"))
			return
		}
		defer m.mutation.Unlock()
	}
	cfg, err := loadConfigMode(m.path, false)
	if err != nil {
		finish(errors.New("Relay configuration is invalid or requires migration"))
		return
	}
	cfg.commandOutput = out
	switch request.Action {
	case "status":
		status, err := readRuntimeStatus(m.path)
		if err == nil {
			fmt.Fprintf(out, "ingest ready: %t\ncontrol connected: %t\nreport return available: %t\nauthentication attention: %t\n", status.IngestReady, status.ControlConnected, status.ReportReturnAvailable, status.AuthenticationAttention)
		}
		finish(err)
	case "doctor":
		err := validateConfig(cfg, "run")
		if err == nil {
			err = doctorTo(cfg, out)
		}
		finish(err)
	case "ready":
		err := checkRuntimeReady(m.path, time.Now())
		if err == nil {
			_, err = readCredentialFile(cfg.CredentialPath, time.Now())
		}
		if err == nil {
			fmt.Fprintln(out, "relay ready")
		}
		finish(err)
	case "rotate-credential":
		finish(rotateCredential(ctx, cfg))
	case "enroll":
		stopped := false
		cfg.beforePairingCommit = func() error {
			stopped = true
			if err := m.transition(ctx, true); err != nil {
				return err
			}
			return ctx.Err()
		}
		err := enroll(ctx, cfg, m.path)
		if stopped {
			err = errors.Join(err, m.resumeAfterPairing(ctx))
		}
		finish(err)
	}
}

func runNativeManagement(ctx context.Context, cfg *config, path string) error {
	if err := requireServiceIdentity(); err != nil {
		return err
	}
	listeners, err := listenManagement()
	if err != nil {
		return err
	}
	return runManagementListeners(ctx, path, listeners)
}

func runManagementListeners(ctx context.Context, path string, listeners []managementListener) error {
	defer func() {
		for _, l := range listeners {
			l.listener.Close()
		}
	}()
	ctx, cancel := context.WithCancel(ctx)
	m := &managementServer{path: path, transitions: make(chan clinicalTransition), lifetime: ctx}
	defer cancel()
	slots := make(chan struct{}, 16)
	var handlers, acceptors sync.WaitGroup
	defer func() {
		cancel()
		for _, l := range listeners {
			l.listener.Close()
		}
		acceptors.Wait()
		handlers.Wait()
	}()
	for _, endpoint := range listeners {
		endpoint := endpoint
		acceptors.Add(1)
		go func() {
			defer acceptors.Done()
			for {
				conn, err := endpoint.listener.Accept()
				if err != nil {
					return
				}
				select {
				case slots <- struct{}{}:
				default:
					conn.Close()
					continue
				}
				handlers.Add(1)
				go func() {
					defer handlers.Done()
					defer func() { <-slots }()
					admin := endpoint.administrator && managementPeerIsAdministrator(conn)
					m.serve(ctx, conn, admin)
				}()
			}
		}()
	}
	var clinicalCancel context.CancelFunc
	var clinicalDone chan error
	stop := func() error {
		if clinicalCancel == nil {
			return nil
		}
		clinicalCancel()
		err := <-clinicalDone
		clinicalCancel = nil
		clinicalDone = nil
		return err
	}
	defer stop()
	start := func() error {
		if clinicalCancel != nil {
			return nil
		}
		loaded, err := loadConfigMode(path, false)
		if err != nil {
			return err
		}
		if !relayIsEnrolled(loaded) {
			return writeRuntimeStatus(path, "unpaired", nil)
		}
		if err := validateConfig(loaded, "run"); err != nil {
			return err
		}
		clinicalCtx, c := context.WithCancel(ctx)
		clinicalCancel = c
		clinicalDone = make(chan error, 1)
		done := clinicalDone
		go func() { done <- runClinicalWithContext(clinicalCtx, loaded, path) }()
		return nil
	}
	if err := start(); err != nil {
		_ = writeRuntimeStatus(path, "configuration_error", err)
	}
	for {
		select {
		case <-ctx.Done():
			for _, l := range listeners {
				l.listener.Close()
			}
			cancel()
			return stop()
		case request := <-m.transitions:
			if request.stop {
				request.result <- stop()
			} else {
				request.result <- start()
			}
		case err := <-clinicalDone:
			clinicalCancel = nil
			clinicalDone = nil
			if err != nil {
				return err
			}
		}
	}
}

type managementListener struct {
	listener      net.Listener
	administrator bool
}
