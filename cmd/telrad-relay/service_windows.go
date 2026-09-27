//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

const windowsServiceName = "TelradRelay"

func runPlatformService(cfg *config) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service session: %w", err)
	}
	if !isService {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return runRelay(ctx, cfg)
	}
	if logger, err := eventlog.Open(windowsServiceName); err == nil {
		defer logger.Close()
		slog.SetDefault(slog.New(slog.NewTextHandler(eventLogWriter{logger}, nil)))
	}
	return svc.Run(windowsServiceName, &relayWindowsService{cfg: cfg})
}

type eventLogWriter struct{ log *eventlog.Log }

func (writer eventLogWriter) Write(message []byte) (int, error) {
	if err := writer.log.Info(1, strings.TrimSpace(string(message))); err != nil {
		return 0, err
	}
	return len(message), nil
}

type relayWindowsService struct{ cfg *config }

func (service *relayWindowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runRelay(ctx, service.cfg) }()
	running := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	statuses <- running
	for {
		select {
		case err := <-done:
			if err != nil {
				slog.Error("relay stopped", "error", err)
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- running
			case svc.Stop, svc.Shutdown:
				statuses <- svc.Status{State: svc.StopPending}
				cancel()
				if err := <-done; err != nil {
					return true, 1
				}
				return false, 0
			}
		}
	}
}

func serviceAction(action string) error {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	sc := filepath.Join(system, "sc.exe")
	run := func(args ...string) error {
		command := exec.Command(sc, args...)
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		return command.Run()
	}
	switch action {
	case "restart":
		_ = run("stop", windowsServiceName)
		return run("start", windowsServiceName)
	default:
		return run(action, windowsServiceName)
	}
}
