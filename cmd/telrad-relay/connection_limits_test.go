package main

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestConnectionLimiterEnforcesGlobalAndProtocolCaps(t *testing.T) {
	cfg := &config{MaxConnections: 2, MaxDicomConnections: 1, MaxHL7Connections: 2}
	limiter := newConnectionLimiter(cfg)
	if !limiter.Acquire("dicom") {
		t.Fatal("first DICOM connection was rejected")
	}
	if limiter.Acquire("dicom") {
		t.Fatal("DICOM protocol cap was not enforced")
	}
	if !limiter.Acquire("hl7") {
		t.Fatal("HL7 connection within the global cap was rejected")
	}
	if limiter.Acquire("hl7") {
		t.Fatal("global connection cap was not enforced")
	}
	limiter.Release("dicom")
	if !limiter.Acquire("hl7") {
		t.Fatal("released global capacity was not reusable")
	}
}

func TestStreamIdleDeadlineClosesSlowPeer(t *testing.T) {
	first, firstPeer := net.Pipe()
	second, secondPeer := net.Pipe()
	defer first.Close()
	defer firstPeer.Close()
	defer second.Close()
	defer secondPeer.Close()
	wrapped, _ := withStreamDeadlines(first, second, 20*time.Millisecond, time.Second)
	buffer := make([]byte, 1)
	_, err := wrapped.Read(buffer)
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		t.Fatalf("idle read error = %v, want timeout", err)
	}
}

func TestStreamWithoutDeadlinesAllowsLongLivedIdleConnection(t *testing.T) {
	first, firstPeer := net.Pipe()
	second, secondPeer := net.Pipe()
	defer first.Close()
	defer firstPeer.Close()
	defer second.Close()
	defer secondPeer.Close()

	wrapped, wrappedSecond := withStreamDeadlines(first, second, 0, 0)
	if wrapped != first || wrappedSecond != second {
		t.Fatal("deadline-free stream was unnecessarily wrapped")
	}
	time.Sleep(50 * time.Millisecond)

	writeDone := make(chan error, 1)
	go func() {
		_, err := firstPeer.Write([]byte("x"))
		writeDone <- err
	}()
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	if _, err := wrapped.Read(buffer); err != nil {
		t.Fatalf("deadline-free read after idle period: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("deadline-free peer write: %v", err)
	}
}

func TestStreamWithoutLifetimeAllowsSustainedActivity(t *testing.T) {
	first, firstPeer := net.Pipe()
	second, secondPeer := net.Pipe()
	defer first.Close()
	defer firstPeer.Close()
	defer second.Close()
	defer secondPeer.Close()

	wrapped, _ := withStreamDeadlines(first, second, 300*time.Millisecond, 0)
	for range 8 {
		writeDone := make(chan error, 1)
		go func() {
			_, err := firstPeer.Write([]byte("x"))
			writeDone <- err
		}()
		buffer := make([]byte, 1)
		if _, err := wrapped.Read(buffer); err != nil {
			t.Fatalf("active stream with no lifetime ended: %v", err)
		}
		if err := <-writeDone; err != nil {
			t.Fatalf("active stream peer write: %v", err)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func TestStreamHardLifetimeStillClosesActivePeer(t *testing.T) {
	first, firstPeer := net.Pipe()
	second, secondPeer := net.Pipe()
	defer first.Close()
	defer firstPeer.Close()
	defer second.Close()
	defer secondPeer.Close()

	wrapped, _ := withStreamDeadlines(first, second, 150*time.Millisecond, 500*time.Millisecond)
	for range 4 {
		writeDone := make(chan error, 1)
		go func() {
			_, err := firstPeer.Write([]byte("x"))
			writeDone <- err
		}()
		buffer := make([]byte, 1)
		if _, err := wrapped.Read(buffer); err != nil {
			t.Fatalf("stream ended before hard lifetime: %v", err)
		}
		if err := <-writeDone; err != nil {
			t.Fatalf("active stream peer write: %v", err)
		}
		time.Sleep(75 * time.Millisecond)
	}

	buffer := make([]byte, 1)
	_, err := wrapped.Read(buffer)
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		t.Fatalf("hard lifetime read error = %v, want timeout", err)
	}
}

func TestClearingTemporaryReadDeadlineRestoresConfiguredIdlePolicy(t *testing.T) {
	connection, peer := net.Pipe()
	defer connection.Close()
	defer peer.Close()

	wrapped, _ := withStreamDeadlines(connection, nil, 40*time.Millisecond, time.Second)
	if err := wrapped.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := wrapped.Read(make([]byte, 1))
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		t.Fatalf("restored idle read error = %v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("restored idle deadline took %s", elapsed)
	}
}

func TestConnectionPoliciesKeepDicomDeadlinesAndAllowUnlimitedHL7(t *testing.T) {
	cfg := &config{}
	applyConnectionDefaults(cfg)

	dicom := connectionPolicyFor(cfg, "dicom")
	if dicom.idle != 5*time.Minute || dicom.lifetime != 2*time.Hour {
		t.Fatalf("DICOM policy = %#v", dicom)
	}
	hl7 := connectionPolicyFor(cfg, "hl7")
	if hl7.idle != 0 || hl7.lifetime != 0 {
		t.Fatalf("default HL7 policy = %#v, want unlimited", hl7)
	}
}

func TestReconnectBackoffIsJitteredBoundedAndResetsAfterHealthySession(t *testing.T) {
	base := 8 * time.Second
	for range 100 {
		delay := jitterReconnectDelay(base)
		if delay < 6*time.Second || delay > 10*time.Second {
			t.Fatalf("jittered delay %s is outside expected bounds", delay)
		}
	}
	if got := nextReconnectBackoff(16*time.Second, false, 0); got != 30*time.Second {
		t.Fatalf("capped backoff = %s", got)
	}
	if got := nextReconnectBackoff(30*time.Second, true, healthySessionThreshold); got != time.Second {
		t.Fatalf("healthy-session backoff = %s", got)
	}
}
