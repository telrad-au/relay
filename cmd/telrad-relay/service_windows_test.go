//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestWindowsServiceCommandsUseExpectedSCContract(t *testing.T) {
	original := executeServiceCommand
	t.Cleanup(func() { executeServiceCommand = original })
	var commands []string
	executeServiceCommand = func(name string, args ...string) error {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil
	}

	for _, run := range []func() error{
		enableAndStartService,
		func() error { return serviceAction("restart") },
		disableService,
		serviceStatus,
	} {
		if err := run(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"sc.exe config " + windowsServiceName + " start= auto",
		"sc.exe start " + windowsServiceName,
		"sc.exe stop " + windowsServiceName,
		"sc.exe start " + windowsServiceName,
		"sc.exe stop " + windowsServiceName,
		"sc.exe config " + windowsServiceName + " start= demand",
		"sc.exe query " + windowsServiceName,
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("service commands = %q, want %q", commands, want)
	}
}

func TestWindowsServiceStateMachineStopsCleanly(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			original := runWindowsService
			t.Cleanup(func() { runWindowsService = original })
			runWindowsService = func(ctx context.Context, _ *config, _ string) error {
				<-ctx.Done()
				return nil
			}
			requests := make(chan svc.ChangeRequest, 2)
			statuses := make(chan svc.Status, 4)
			result := make(chan struct {
				serviceSpecific bool
				exitCode        uint32
			}, 1)
			service := &relayWindowsService{cfg: defaultConfig(), configPath: "relay.json"}
			go func() {
				serviceSpecific, exitCode := service.Execute(nil, requests, statuses)
				result <- struct {
					serviceSpecific bool
					exitCode        uint32
				}{serviceSpecific, exitCode}
			}()

			requireWindowsServiceState(t, statuses, svc.StartPending)
			requireWindowsServiceState(t, statuses, svc.Running)
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			requireWindowsServiceState(t, statuses, svc.Running)
			requests <- svc.ChangeRequest{Cmd: command}
			requireWindowsServiceState(t, statuses, svc.StopPending)
			select {
			case got := <-result:
				if got.serviceSpecific || got.exitCode != 0 {
					t.Fatalf("service result = %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("Windows service did not stop")
			}
		})
	}
}

func TestWindowsServiceReportsRelayFailure(t *testing.T) {
	original := runWindowsService
	t.Cleanup(func() { runWindowsService = original })
	runWindowsService = func(context.Context, *config, string) error { return errors.New("relay failed") }
	statuses := make(chan svc.Status, 2)
	serviceSpecific, exitCode := (&relayWindowsService{cfg: defaultConfig(), configPath: "relay.json"}).Execute(nil, make(chan svc.ChangeRequest), statuses)
	if !serviceSpecific || exitCode != 1 {
		t.Fatalf("service result = %t, %d", serviceSpecific, exitCode)
	}
	requireWindowsServiceState(t, statuses, svc.StartPending)
	requireWindowsServiceState(t, statuses, svc.Running)
}

func TestWindowsForegroundDelegatesToSignalAwareRunner(t *testing.T) {
	isService, err := svc.IsWindowsService()
	if err != nil {
		t.Fatal(err)
	}
	if isService {
		t.Skip("test process is running as a Windows service")
	}
	original := runWindowsForeground
	t.Cleanup(func() { runWindowsForeground = original })
	wantErr := errors.New("foreground stopped")
	cfg := defaultConfig()
	configPath := filepath.Join(t.TempDir(), "relay.json")
	runWindowsForeground = func(gotConfig *config, gotPath string) error {
		if gotConfig != cfg || gotPath != configPath {
			t.Fatalf("foreground inputs = %p, %q", gotConfig, gotPath)
		}
		return wantErr
	}
	if err := runPlatformService(cfg, configPath); !errors.Is(err, wantErr) {
		t.Fatalf("foreground error = %v", err)
	}
}

func requireWindowsServiceState(t *testing.T, statuses <-chan svc.Status, want svc.State) {
	t.Helper()
	select {
	case status := <-statuses:
		if status.State != want {
			t.Fatalf("service state = %v, want %v", status.State, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("service did not report state %v", want)
	}
}
