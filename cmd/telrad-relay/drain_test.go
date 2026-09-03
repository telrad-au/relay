package main

import (
	"testing"
	"time"
)

func TestWorkDrainerRejectsNewWorkAndWaitsForActiveWork(t *testing.T) {
	drainer := newWorkDrainer()
	if !drainer.Start() || !drainer.Start() {
		t.Fatal("drainer rejected work before draining began")
	}
	drained := drainer.BeginDrain()
	if drainer.Start() {
		t.Fatal("drainer accepted new work after draining began")
	}
	drainer.Done()
	select {
	case <-drained:
		t.Fatal("drainer completed while work remained")
	default:
	}
	drainer.Done()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drainer did not complete after all work finished")
	}
}
