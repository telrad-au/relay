package main

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	pickupMinimumBackoff = time.Second
	pickupMaximumBackoff = 60 * time.Second
	pickupHealthyPeriod  = time.Minute
	pickupWriteTimeout   = 30 * time.Second

	refusalText         = "Accession not ordered through this Relay"
	receiverFailureText = "Report receiver unavailable"
)

var errPickupInterleaved = errors.New("report pickup protocol violation")

// runPickup keeps one outbound connection to Telrad's report port. Telrad
// writes a framed ORU; Relay answers one framed acknowledgement. The
// connection is re-established with backoff, and after a certificate renewal.
// When ctx ends, a report already being handled is finished and acknowledged
// before runPickup returns.
func (r *relay) runPickup(ctx context.Context) {
	backoff := pickupMinimumBackoff
	for ctx.Err() == nil {
		// A change signalled before this dial is already reflected in it.
		select {
		case <-r.store.changed():
		default:
		}
		conn, err := r.dialTelrad(ctx, r.store.endpoints().ReportPort, "report")
		if err != nil {
			r.status.pickupState(false, safeNetworkError(err).Error())
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, pickupMaximumBackoff)
			continue
		}
		r.status.pickupState(true, "")
		started := time.Now()
		connCtx, stopConn := context.WithCancel(ctx)
		renewed := make(chan struct{})
		go func() {
			select {
			case <-r.store.changed():
				close(renewed)
				stopConn()
			case <-connCtx.Done():
			}
		}()
		err = r.servePickup(connCtx, conn)
		stopConn()
		_ = conn.Close()
		select {
		case <-renewed:
			err = nil // a planned reconnect, not a failure
			backoff = pickupMinimumBackoff
		default:
		}
		message := ""
		if err != nil && ctx.Err() == nil {
			message = safeNetworkError(err).Error()
		}
		r.status.pickupState(false, message)
		if time.Since(started) >= pickupHealthyPeriod {
			backoff = pickupMinimumBackoff
		}
		if !sleepContext(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, pickupMaximumBackoff)
	}
}

// servePickup answers reports until the connection fails or ctx ends. Ending
// ctx interrupts only the wait for the next report; a report whose frame has
// been read is handled and acknowledged first.
func (r *relay) servePickup(ctx context.Context, conn net.Conn) error {
	reader := bufio.NewReaderSize(conn, 32*1024)
	frameTimeout := time.Duration(r.cfg.HL7FrameSeconds) * time.Second
	// mu orders read-deadline changes against the stop so a stop is never
	// overwritten by the loop re-arming its own deadline.
	var mu sync.Mutex
	stopped := false
	setReadDeadline := func(deadline time.Time) bool {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return false
		}
		_ = conn.SetReadDeadline(deadline)
		return true
	}
	defer context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		_ = conn.SetReadDeadline(time.Now())
	})()
	for {
		if !setReadDeadline(time.Time{}) {
			return ctx.Err()
		}
		if _, err := reader.Peek(1); err != nil {
			return err
		}
		if !setReadDeadline(time.Now().Add(frameTimeout)) {
			return ctx.Err()
		}
		frame, err := readMLLPFrame(reader, r.cfg.HL7MaxBytes)
		if err != nil {
			return err
		}
		// Telrad must wait for this acknowledgement before the next report.
		if reader.Buffered() > 0 {
			return errPickupInterleaved
		}
		ack, err := r.handleReport(context.WithoutCancel(ctx), unframe(frame))
		if err != nil {
			return err
		}
		_ = conn.SetWriteDeadline(time.Now().Add(pickupWriteTimeout))
		if _, err := conn.Write(frameMessage(ack)); err != nil {
			return err
		}
	}
}

// handleReport is the gate. It returns the acknowledgement to send Telrad:
// the receiver's own reply when the report was authorised and delivered, or
// one Relay composed. An unparseable report is a protocol error.
func (r *relay) handleReport(ctx context.Context, message []byte) ([]byte, error) {
	report, err := parseHL7(message)
	if err != nil {
		return nil, err
	}
	if report.controlID() == "" {
		return nil, errors.New("report has no MSH-10")
	}
	accessions := report.accessions()
	authorised := len(accessions) > 0
	for _, accession := range accessions {
		if !r.ledger.contains(accession) {
			authorised = false
			break
		}
	}
	if !authorised {
		r.status.reportRefused()
		slog.Warn("report refused: accession not in ledger")
		return composeAck(report, "AR", refusalText), nil
	}
	reply, err := r.deliverReport(ctx, report, message)
	if err != nil {
		r.status.reportFailed(err)
		slog.Warn("report delivery to receiver failed", "error", err)
		return composeAck(report, "AE", receiverFailureText), nil
	}
	r.status.reportDelivered()
	return reply, nil
}

var errInvalidReceiverAck = errors.New("invalid receiver acknowledgement")

// deliverReport sends the report to the clinic receiver and returns the
// receiver's unframed acknowledgement exactly as received.
func (r *relay) deliverReport(ctx context.Context, report *hl7Message, message []byte) ([]byte, error) {
	dialer := &net.Dialer{Timeout: time.Duration(r.cfg.ConnectTimeoutSeconds) * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(r.cfg.ReportHost, strconv.Itoa(r.cfg.ReportPort)))
	if err != nil {
		return nil, safeNetworkError(err)
	}
	defer conn.Close()
	ackTimeout := time.Duration(r.cfg.ReceiverAckSeconds) * time.Second
	_ = conn.SetDeadline(time.Now().Add(ackTimeout))
	if _, err := conn.Write(frameMessage(message)); err != nil {
		return nil, safeNetworkError(err)
	}
	frame, err := readMLLPFrame(bufio.NewReaderSize(conn, 32*1024), r.cfg.HL7MaxBytes)
	if err != nil {
		return nil, safeNetworkError(err)
	}
	reply := unframe(frame)
	parsed, err := parseHL7(reply)
	if err != nil {
		return nil, errInvalidReceiverAck
	}
	if _, controlID, ok := parsed.acknowledgement(); !ok || controlID != report.controlID() {
		return nil, errInvalidReceiverAck
	}
	return reply, nil
}
