//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

var (
	runWindowsForeground = run
	runWindowsService    = runWithContext
)

func runPlatformService(cfg *config, configPath string) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service session: %w", err)
	}
	if !isService {
		return runWindowsForeground(cfg, configPath)
	}
	eventLogger, err := eventlog.Open(windowsServiceName)
	if err != nil {
		return fmt.Errorf("open Windows Application event log: %w", err)
	}
	defer eventLogger.Close()
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(eventLogWriter{log: eventLogger}, nil)))
	defer slog.SetDefault(previousLogger)
	return svc.Run(windowsServiceName, &relayWindowsService{cfg: cfg, configPath: configPath})
}

type eventLogWriter struct {
	log *eventlog.Log
}

func (writer eventLogWriter) Write(message []byte) (int, error) {
	if err := writer.log.Info(1, strings.TrimSpace(string(message))); err != nil {
		return 0, err
	}
	return len(message), nil
}

var _ io.Writer = eventLogWriter{}

type relayWindowsService struct {
	cfg        *config
	configPath string
}

func (service *relayWindowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	statuses <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runWindowsService(ctx, service.cfg, service.configPath)
	}()
	current := svc.Status{State: svc.Running, Accepts: accepted}
	statuses <- current
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- current
			case svc.Stop, svc.Shutdown:
				current = svc.Status{State: svc.StopPending}
				statuses <- current
				cancel()
				if err := <-done; err != nil {
					return true, 1
				}
				return false, 0
			}
		}
	}
}
