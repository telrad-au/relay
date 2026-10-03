package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	tcpKeepAlive        = 30 * time.Second
	serviceDrainTimeout = 90 * time.Second
)

// relay holds everything the listeners and the report pickup share.
type relay struct {
	cfg    *config
	store  *identityStore
	ledger *ledger
	status *statusServer
	work   sync.WaitGroup
}

func (r *relay) dialTelrad(ctx context.Context, port int, purpose string) (net.Conn, error) {
	endpoints := r.store.endpoints()
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: time.Duration(r.cfg.ConnectTimeoutSeconds) * time.Second, KeepAlive: tcpKeepAlive},
		Config:    r.store.clientTLS(endpoints.Host),
	}
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Duration(r.cfg.ConnectTimeoutSeconds)*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(dialCtx, "tcp", endpoints.address(port))
	if err != nil {
		r.status.telradFailure(purpose, safeNetworkError(err))
		return nil, err
	}
	r.status.telradConnected(purpose)
	return conn, nil
}

// serveListener accepts until the listener closes. Connections beyond limit
// are closed at accept; every served connection is tracked for draining.
func (r *relay) serveListener(ctx context.Context, listener net.Listener, limit int, protocol string, handle func(context.Context, net.Conn)) {
	slots := make(chan struct{}, limit)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				slog.Warn("listener accept failed", "protocol", protocol, "error", safeNetworkError(err))
				if !sleepContext(ctx, 100*time.Millisecond) {
					return
				}
				continue
			}
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			r.status.connectionRefused(protocol)
			_ = conn.Close()
			continue
		}
		r.work.Add(1)
		r.status.connectionOpened(protocol)
		go func() {
			defer r.work.Done()
			defer r.status.connectionClosed(protocol)
			defer func() { <-slots }()
			defer conn.Close()
			handle(ctx, conn)
		}()
	}
}

// serveDICOM is a byte pipe: the PACS negotiates directly with Telrad's SCP.
func (r *relay) serveDICOM(ctx context.Context, clinic net.Conn) {
	upstream, err := r.dialTelrad(ctx, r.store.endpoints().DicomPort, "dicom")
	if err != nil {
		return
	}
	defer upstream.Close()
	pipe(ctx, clinic, upstream, time.Duration(r.cfg.IdleTimeoutSeconds)*time.Second)
}

// pipe copies bytes in both directions until either side finishes. A half
// close is propagated so DICOM release and MLLP close semantics survive. Both
// connections close when the pipe returns or ctx ends.
func pipe(ctx context.Context, first, second net.Conn, idle time.Duration) {
	deadlines := newIdleDeadline(idle, first, second)
	deadlines.touch()
	var wg sync.WaitGroup
	copyDirection := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(&activityWriter{dst, deadlines}, src)
		if err == nil {
			closeWrite(dst)
			return
		}
		_ = first.Close()
		_ = second.Close()
	}
	wg.Add(2)
	go copyDirection(first, second)
	go copyDirection(second, first)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		_ = first.Close()
		_ = second.Close()
		<-done
	}
}

func closeWrite(conn net.Conn) {
	if closer, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
	}
}

type idleDeadline struct {
	idle  time.Duration
	conns []net.Conn
}

func newIdleDeadline(idle time.Duration, conns ...net.Conn) *idleDeadline {
	return &idleDeadline{idle: idle, conns: conns}
}

func (d *idleDeadline) touch() {
	if d.idle <= 0 {
		return
	}
	deadline := time.Now().Add(d.idle)
	for _, conn := range d.conns {
		_ = conn.SetDeadline(deadline)
	}
}

type activityWriter struct {
	net.Conn
	deadlines *idleDeadline
}

func (w *activityWriter) Write(data []byte) (int, error) {
	count, err := w.Conn.Write(data)
	if count > 0 {
		w.deadlines.touch()
	}
	return count, err
}

// pendingOrder is what Relay remembers about a forwarded message until Telrad
// answers it: enough to decide whether an AA creates ledger entries.
type pendingOrder struct {
	orderControl string
	accessions   []string
}

// serveHL7 forwards MLLP frames both ways and records accepted orders. Frames
// are forwarded byte for byte; correlation is by MSH-10 and MSA-2 only.
func (r *relay) serveHL7(ctx context.Context, clinic net.Conn) {
	upstream, err := r.dialTelrad(ctx, r.store.endpoints().HL7Port, "hl7")
	if err != nil {
		return
	}
	defer upstream.Close()

	idle := time.Duration(r.cfg.IdleTimeoutSeconds) * time.Second
	frameTimeout := time.Duration(r.cfg.HL7FrameSeconds) * time.Second
	ackTimeout := time.Duration(r.cfg.TelradAckSeconds) * time.Second
	closeBoth := func() { _ = clinic.Close(); _ = upstream.Close() }

	// mu guards pending and every change to the upstream read deadline, so
	// the deadline always reflects whether an answer is still outstanding.
	var mu sync.Mutex
	pending := make(map[string]pendingOrder)
	outstanding := 0
	setUpstreamDeadline := func() {
		if outstanding > 0 {
			_ = upstream.SetReadDeadline(time.Now().Add(ackTimeout))
		} else {
			_ = upstream.SetReadDeadline(time.Time{})
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer closeBoth()
		reader := bufio.NewReaderSize(clinic, 32*1024)
		for {
			frame, err := readMLLPFrameWithDeadlines(clinic, reader, r.cfg.HL7MaxBytes, idle, frameTimeout)
			if err != nil {
				return
			}
			mu.Lock()
			if message, err := parseHL7(unframe(frame)); err == nil {
				if controlID := message.controlID(); controlID != "" {
					pending[controlID] = pendingOrder{orderControl: message.orderControl(), accessions: recordableAccessions(message.accessions())}
				}
			}
			outstanding++
			setUpstreamDeadline()
			mu.Unlock()
			_ = upstream.SetWriteDeadline(time.Now().Add(frameTimeout))
			if _, err := upstream.Write(frame); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer closeBoth()
		reader := bufio.NewReaderSize(upstream, 32*1024)
		for {
			// Waiting for a reply is bounded by the acknowledgement deadline
			// only while one is outstanding; a started frame by the frame deadline.
			if _, err := reader.Peek(1); err != nil {
				return
			}
			mu.Lock()
			_ = upstream.SetReadDeadline(time.Now().Add(frameTimeout))
			mu.Unlock()
			frame, err := readMLLPFrame(reader, r.cfg.HL7MaxBytes)
			if err != nil {
				return
			}
			var order pendingOrder
			var known bool
			var code string
			mu.Lock()
			if message, err := parseHL7(unframe(frame)); err == nil {
				var controlID string
				var ok bool
				if code, controlID, ok = message.acknowledgement(); ok {
					order, known = pending[controlID]
					delete(pending, controlID)
					outstanding = max(outstanding-1, 0)
				}
			}
			setUpstreamDeadline()
			mu.Unlock()
			if known && code == "AA" && isOrderPlacement(order.orderControl) && len(order.accessions) > 0 {
				// Durable before the RIS learns the order was accepted.
				if err := r.recordAccessions(order.accessions); err != nil {
					slog.Error("ledger append failed; closing HL7 connection", "error", err)
					return
				}
			}
			_ = clinic.SetWriteDeadline(time.Now().Add(frameTimeout))
			if _, err := clinic.Write(frame); err != nil {
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		closeBoth()
		<-done
	}
}

// recordableAccessions keeps the OBR-18 values the ledger can hold: UTF-8
// without line breaks. A report naming any other value is never authorised.
func recordableAccessions(values []string) []string {
	var recordable []string
	for _, value := range values {
		if utf8.ValidString(value) {
			recordable = append(recordable, value)
		}
	}
	return recordable
}
