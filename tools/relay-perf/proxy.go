package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// This test-only TCP hop preserves end-to-end TLS. Shaping its cloud-facing
// connection lets native clients use the same two egress queues as Docker
// clients, without requiring IFB drivers or changing the native host's network.
func serveProxy(ctx context.Context) error {
	listener, err := net.Listen("tcp", ":8443")
	if err != nil {
		return errors.New("synthetic TLS proxy listener failed")
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		incoming, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("synthetic TLS proxy accept failed")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer incoming.Close()
			outgoing, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", "cloud:8443")
			if err != nil {
				return
			}
			defer outgoing.Close()
			proxyStream(ctx, incoming, outgoing)
		}()
	}
}

func proxyStream(ctx context.Context, a, b net.Conn) {
	stop := context.AfterFunc(ctx, func() { _ = a.Close(); _ = b.Close() })
	defer stop()
	done := make(chan struct{})
	copyHalf := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go func() { defer close(done); copyHalf(b, a) }()
	copyHalf(a, b)
	<-done
}
