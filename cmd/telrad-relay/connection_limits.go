package main

import (
	"log/slog"
	"net"
	"sync"
	"time"
)

type connectionLimiter struct {
	global       chan struct{}
	dicom        chan struct{}
	hl7          chan struct{}
	mu           sync.Mutex
	rejected     map[string]int
	lastReported map[string]time.Time
}

func newConnectionLimiter(cfg *config) *connectionLimiter {
	return &connectionLimiter{
		global:       make(chan struct{}, cfg.MaxConnections),
		dicom:        make(chan struct{}, cfg.MaxDicomConnections),
		hl7:          make(chan struct{}, cfg.MaxHL7Connections),
		rejected:     make(map[string]int),
		lastReported: make(map[string]time.Time),
	}
}

func (limiter *connectionLimiter) protocol(protocol string) chan struct{} {
	if protocol == "dicom" {
		return limiter.dicom
	}
	return limiter.hl7
}

func (limiter *connectionLimiter) Acquire(protocol string) bool {
	select {
	case limiter.global <- struct{}{}:
	default:
		return false
	}
	protocolLimit := limiter.protocol(protocol)
	select {
	case protocolLimit <- struct{}{}:
		return true
	default:
		<-limiter.global
		return false
	}
}

func (limiter *connectionLimiter) Release(protocol string) {
	<-limiter.protocol(protocol)
	<-limiter.global
}

func (limiter *connectionLimiter) RecordOverload(protocol string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.rejected[protocol]++
	now := time.Now()
	if limiter.lastReported[protocol].IsZero() || now.Sub(limiter.lastReported[protocol]) >= time.Minute {
		slog.Warn("clinic connection limit reached", "protocol", protocol, "rejectedSinceLastReport", limiter.rejected[protocol])
		limiter.rejected[protocol] = 0
		limiter.lastReported[protocol] = now
	}
}

type connectionPolicy struct {
	idle     time.Duration
	lifetime time.Duration
}

func connectionPolicyFor(cfg *config, protocol string) connectionPolicy {
	if protocol == "dicom" {
		return connectionPolicy{time.Duration(cfg.DicomIdleTimeoutSeconds) * time.Second, time.Duration(cfg.DicomLifetimeSeconds) * time.Second}
	}
	return connectionPolicy{time.Duration(cfg.HL7IdleTimeoutSeconds) * time.Second, time.Duration(cfg.HL7LifetimeSeconds) * time.Second}
}

type streamDeadline struct {
	mu       sync.Mutex
	peers    []net.Conn
	idle     time.Duration
	deadline time.Time
}

func withStreamDeadlines(first, second net.Conn, idle, lifetime time.Duration) (net.Conn, net.Conn) {
	if idle <= 0 && lifetime <= 0 {
		return first, second
	}
	deadlines := &streamDeadline{peers: []net.Conn{first, second}, idle: idle}
	if lifetime > 0 {
		deadlines.deadline = time.Now().Add(lifetime)
	}
	deadlines.touch()
	return &activityConn{Conn: first, deadlines: deadlines}, &activityConn{Conn: second, deadlines: deadlines}
}

func (deadlines *streamDeadline) touch() {
	deadlines.mu.Lock()
	defer deadlines.mu.Unlock()
	var next time.Time
	if deadlines.idle > 0 {
		next = time.Now().Add(deadlines.idle)
	}
	if !deadlines.deadline.IsZero() && (next.IsZero() || next.After(deadlines.deadline)) {
		next = deadlines.deadline
	}
	for _, peer := range deadlines.peers {
		if peer != nil {
			_ = peer.SetDeadline(next)
		}
	}
}

func (deadlines *streamDeadline) clamp(value time.Time) time.Time {
	if value.IsZero() && deadlines.idle > 0 {
		value = time.Now().Add(deadlines.idle)
	}
	if !deadlines.deadline.IsZero() && (value.IsZero() || value.After(deadlines.deadline)) {
		return deadlines.deadline
	}
	return value
}

type activityConn struct {
	net.Conn
	deadlines *streamDeadline
}

func (connection *activityConn) Read(buffer []byte) (int, error) {
	count, err := connection.Conn.Read(buffer)
	if count > 0 && connection.deadlines.idle > 0 {
		connection.deadlines.touch()
	}
	return count, err
}

func (connection *activityConn) Write(buffer []byte) (int, error) {
	count, err := connection.Conn.Write(buffer)
	if count > 0 && connection.deadlines.idle > 0 {
		connection.deadlines.touch()
	}
	return count, err
}

func (connection *activityConn) SetDeadline(deadline time.Time) error {
	return connection.Conn.SetDeadline(connection.deadlines.clamp(deadline))
}

func (connection *activityConn) SetReadDeadline(deadline time.Time) error {
	return connection.Conn.SetReadDeadline(connection.deadlines.clamp(deadline))
}

func (connection *activityConn) SetWriteDeadline(deadline time.Time) error {
	return connection.Conn.SetWriteDeadline(connection.deadlines.clamp(deadline))
}

func (connection *activityConn) CloseWrite() error {
	if closeWriter, ok := connection.Conn.(interface{ CloseWrite() error }); ok {
		return closeWriter.CloseWrite()
	}
	return nil
}
