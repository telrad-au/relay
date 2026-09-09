package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestProxyPreservesBytesAndCancelsBlockedTransfers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, incoming := net.Pipe()
	outgoing, cloud := net.Pipe()
	defer client.Close()
	defer cloud.Close()
	defer incoming.Close()
	defer outgoing.Close()
	done := make(chan struct{})
	go func() { defer close(done); proxyStream(ctx, incoming, outgoing) }()
	for _, direction := range []struct{ from, to net.Conn }{{client, cloud}, {cloud, client}} {
		written := make(chan error, 1)
		go func() { _, err := direction.from.Write([]byte("synthetic TLS bytes")); written <- err }()
		b := make([]byte, len("synthetic TLS bytes"))
		if _, err := io.ReadFull(direction.to, b); err != nil || string(b) != "synthetic TLS bytes" {
			t.Fatalf("proxy changed bytes: %q %v", b, err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled proxy retained a blocked connection")
	}
}
