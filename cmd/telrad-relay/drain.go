package main

import "sync"

type workDrainer struct {
	mu        sync.Mutex
	accepting bool
	active    int
	drained   chan struct{}
}

func newWorkDrainer() *workDrainer {
	drained := make(chan struct{})
	close(drained)
	return &workDrainer{accepting: true, drained: drained}
}

func (drainer *workDrainer) Start() bool {
	drainer.mu.Lock()
	defer drainer.mu.Unlock()
	if !drainer.accepting {
		return false
	}
	if drainer.active == 0 {
		drainer.drained = make(chan struct{})
	}
	drainer.active++
	return true
}

func (drainer *workDrainer) Done() {
	drainer.mu.Lock()
	defer drainer.mu.Unlock()
	if drainer.active <= 0 {
		panic("relay work drainer count became negative")
	}
	drainer.active--
	if drainer.active == 0 {
		close(drainer.drained)
	}
}

func (drainer *workDrainer) BeginDrain() <-chan struct{} {
	drainer.mu.Lock()
	defer drainer.mu.Unlock()
	drainer.accepting = false
	return drainer.drained
}

func (drainer *workDrainer) Active() int {
	drainer.mu.Lock()
	defer drainer.mu.Unlock()
	return drainer.active
}
