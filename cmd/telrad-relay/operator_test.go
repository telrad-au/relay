package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService records service actions instead of calling systemd or the SCM.
type fakeService struct {
	mu        sync.Mutex
	isRunning bool
	actions   []string
	// onStart runs after start or restart, for example to change status.
	onStart func()
}

func (service *fakeService) record(action string) {
	service.mu.Lock()
	service.actions = append(service.actions, action)
	service.mu.Unlock()
}

func (service *fakeService) running() (bool, error) { return service.isRunning, nil }

func (service *fakeService) start() error {
	service.record("start")
	service.isRunning = true
	if service.onStart != nil {
		service.onStart()
	}
	return nil
}

func (service *fakeService) stop() error {
	service.record("stop")
	service.isRunning = false
	return nil
}

func (service *fakeService) restart() error {
	service.record("restart")
	service.isRunning = true
	if service.onStart != nil {
		service.onStart()
	}
	return nil
}

func (service *fakeService) history() string {
	service.mu.Lock()
	defer service.mu.Unlock()
	return strings.Join(service.actions, ",")
}

// fakeStatus serves a status report the test changes as the fake service acts.
type fakeStatus struct {
	mu     sync.Mutex
	report *statusReport // nil: the service is not answering
	reads  int
}

func (status *fakeStatus) set(report *statusReport) {
	status.mu.Lock()
	status.report = report
	status.mu.Unlock()
}

func (status *fakeStatus) read() (*statusReport, error) {
	status.mu.Lock()
	defer status.mu.Unlock()
	status.reads++
	if status.report == nil {
		return nil, errors.New("the Relay service is not running or its status endpoint is unreachable")
	}
	copied := *status.report
	return &copied, nil
}

type testOperator struct {
	*operatorEnv
	output  *bytes.Buffer
	service *fakeService
	status  *fakeStatus
	// prompts collects what was asked on the fake terminal.
	prompts *bytes.Buffer
}

// newTestOperator is an elevated operator with no terminal, a stopped fake
// service and a status endpoint that does not answer.
func newTestOperator(t *testing.T) *testOperator {
	t.Helper()
	operator := &testOperator{output: &bytes.Buffer{}, service: &fakeService{}, status: &fakeStatus{}, prompts: &bytes.Buffer{}}
	operator.operatorEnv = &operatorEnv{
		out: operator.output, openTerminal: func() *terminal { return nil }, elevated: func() bool { return true },
		service: operator.service, status: operator.status.read,
		statusWait: time.Second, statusPoll: 100 * time.Millisecond, sleep: func(time.Duration) {},
	}
	return operator
}

// answer gives the operator a terminal on which the next question is answered.
func (operator *testOperator) answer(reply string) {
	operator.openTerminal = func() *terminal {
		return &terminal{in: strings.NewReader(reply), out: operator.prompts, close: func() error { return nil }}
	}
}

func TestConfirm(t *testing.T) {
	operator := newTestOperator(t)
	if ok, err := operator.confirm("Proceed?", true); !ok || err != nil {
		t.Fatalf("--yes: %t %v", ok, err)
	}
	if ok, err := operator.confirm("Proceed?", false); ok || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("no terminal: %t %v", ok, err)
	}
	for reply, want := range map[string]bool{"y\n": true, "YES\n": true, " yes \r\n": true, "n\n": false, "\n": false, "": false, "sure\n": false} {
		operator.answer(reply)
		operator.prompts.Reset()
		if ok, err := operator.confirm("Proceed?", false); ok != want || err != nil {
			t.Fatalf("reply %q: %t %v", reply, ok, err)
		}
		if operator.prompts.String() != "Proceed? [y/N] " {
			t.Fatalf("prompt=%q", operator.prompts.String())
		}
	}
}

func TestRequireElevation(t *testing.T) {
	operator := newTestOperator(t)
	operator.elevated = func() bool { return false }
	err := operator.requireElevation("pair")
	if err == nil || !strings.Contains(err.Error(), "telrad pair changes the installation") || !strings.Contains(err.Error(), elevationHint) {
		t.Fatalf("err=%v", err)
	}
}
