package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Operator commands (pair, report-receiver, uninstall) change a native
// installation. Everything they touch outside the process is reached through
// operatorEnv so the logic runs in tests without root, systemd or the Windows
// Service Control Manager.

// serviceControl manages the installed Relay service.
type serviceControl interface {
	running() (bool, error)
	start() error
	stop() error
	restart() error
}

// terminal is where a confirmation question is asked and answered.
type terminal struct {
	in    io.Reader
	out   io.Writer
	close func() error
}

type operatorEnv struct {
	out io.Writer
	// openTerminal returns nil when no one can answer a question.
	openTerminal func() *terminal
	// elevated reports whether the process may change the installation.
	elevated func() bool
	service  serviceControl
	status   func() (*statusReport, error)
	// statusWait bounds how long a command waits for the restarted service.
	statusWait time.Duration
	statusPoll time.Duration
	sleep      func(time.Duration)
}

func newOperatorEnv(statusAddress string, out io.Writer) *operatorEnv {
	return &operatorEnv{
		out: out, openTerminal: openTerminal, elevated: isElevated, service: nativeService(),
		status:     func() (*statusReport, error) { return fetchStatus(statusAddress) },
		statusWait: 30 * time.Second, statusPoll: 500 * time.Millisecond, sleep: time.Sleep,
	}
}

// requireElevation refuses a command that changes the installation unless the
// process runs as root on Linux or elevated on Windows.
func (env *operatorEnv) requireElevation(command string) error {
	if env.elevated() {
		return nil
	}
	return fmt.Errorf("telrad %s changes the installation; %s", command, elevationHint)
}

// confirm asks question on the terminal and reports whether the answer was
// yes. yes answers it in advance; without a terminal the command is refused.
func (env *operatorEnv) confirm(question string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	console := env.openTerminal()
	if console == nil {
		return false, errors.New("no terminal is available to answer the confirmation question; rerun with --yes")
	}
	defer console.close()
	fmt.Fprintf(console.out, "%s [y/N] ", question)
	answer, _ := bufio.NewReader(console.in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// waitForStatus polls the status endpoint until done accepts a report or the
// wait ends. It returns the last report read, which may be nil.
func (env *operatorEnv) waitForStatus(done func(*statusReport) bool) (*statusReport, bool) {
	var last *statusReport
	for waited := time.Duration(0); ; waited += env.statusPoll {
		if report, err := env.status(); err == nil {
			last = report
			if done(report) {
				return report, true
			}
		}
		if waited >= env.statusWait {
			return last, false
		}
		env.sleep(env.statusPoll)
	}
}
