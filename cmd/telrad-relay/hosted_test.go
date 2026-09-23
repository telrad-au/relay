package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type hostedTestConn struct {
	net.Conn
	peer net.Addr
}

func (conn hostedTestConn) RemoteAddr() net.Addr { return conn.peer }

type hostedTestListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (listener *hostedTestListener) Accept() (net.Conn, error) {
	select {
	case conn := <-listener.connections:
		return conn, nil
	case <-listener.closed:
		return nil, net.ErrClosed
	}
}

func (listener *hostedTestListener) Close() error {
	listener.once.Do(func() { close(listener.closed) })
	return nil
}

func (listener *hostedTestListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}
}

func TestHostedHL7RoutesByObservedPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bindings := map[string]*hostedBinding{}
	received := make(chan string, 2)
	for index, ip := range []string{"100.96.0.10", "100.96.0.11"} {
		index, ip := index, ip
		server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			received <- ip + ":" + string(body)
			writer.Header().Set("Content-Type", "application/hl7-v2")
			_, _ = writer.Write([]byte("MSA|AA|control-1\r"))
		}))
		defer server.Close()
		cfg := defaultConfig()
		cfg.HL7URL = server.URL
		bindingCtx, stop := context.WithCancel(ctx)
		bindings[ip] = &hostedBinding{
			cfg: cfg, provider: testProvider(t, testCredential(byte('A'+index))),
			clients: protocolClients{secure: server.Client()}, status: newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")),
			limits: newConnectionLimiter(cfg), ctx: bindingCtx, cancel: stop, sockets: make(map[net.Conn]struct{}),
		}
	}
	listener := &hostedTestListener{connections: make(chan net.Conn, 3), closed: make(chan struct{})}
	work := newWorkDrainer()
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		acceptHostedConnections(ctx, "hl7", listener, bindings, work, newConnectionLimiter(defaultConfig()), errCh)
	}()
	message := []byte("MSH|^~\\&|CLINIC|A|TELRAD|B|20260101000000||ORU^R01|control-1|P|2.5\r")
	for _, ip := range []string{"100.96.0.10", "100.96.0.11"} {
		clinic, relay := net.Pipe()
		listener.connections <- hostedTestConn{Conn: relay, peer: &net.TCPAddr{IP: net.ParseIP(ip)}}
		frame := append(append([]byte{mllpStart}, message...), mllpEnd, mllpCR)
		if _, err := clinic.Write(frame); err != nil {
			t.Fatal(err)
		}
		_ = clinic.SetReadDeadline(time.Now().Add(3 * time.Second))
		ack, err := readMLLPFrame(clinic, 1024)
		if err != nil || !bytes.Equal(ack, []byte("\x0bMSA|AA|control-1\r\x1c\r")) {
			t.Fatalf("ACK=%q error=%v", ack, err)
		}
		_ = clinic.Close()
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case result := <-received:
			seen[result] = true
		case <-time.After(3 * time.Second):
			t.Fatal("hosted child did not forward HL7")
		}
	}
	for _, ip := range []string{"100.96.0.10", "100.96.0.11"} {
		if !seen[ip+":"+string(message)] {
			t.Fatalf("binding %s did not use its own HTTPS client", ip)
		}
	}
	unknownClinic, unknownRelay := net.Pipe()
	listener.connections <- hostedTestConn{Conn: unknownRelay, peer: &net.TCPAddr{IP: net.ParseIP("100.96.0.12")}}
	_ = unknownClinic.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := unknownClinic.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("unknown peer was not closed: %v", err)
	}
	_ = unknownClinic.Close()
	_ = listener.Close()
	<-done
	for _, binding := range bindings {
		binding.stop()
	}
	if err := drainRelayWork(work); err != nil {
		t.Fatal(err)
	}
}

func TestHostedStopClosesIdleSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	binding := &hostedBinding{ctx: ctx, cancel: cancel, status: newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")), sockets: make(map[net.Conn]struct{})}
	clinic, relay := net.Pipe()
	defer clinic.Close()
	if !binding.register(relay) {
		t.Fatal("failed to register socket")
	}
	binding.stop()
	_ = clinic.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := clinic.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("idle socket remained open: %v", err)
	}
	if binding.register(relay) {
		t.Fatal("closed binding admitted a socket")
	}
}
