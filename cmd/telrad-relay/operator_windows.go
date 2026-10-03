//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	elevationHint       = "run it from PowerShell opened as Administrator"
	serviceDisplayName  = "the " + windowsServiceName + " service"
	reportReceiverUsage = "telrad report-receiver HOST[:PORT] (as Administrator)"
	// The service drains in-flight work for up to 90 seconds when stopped.
	windowsStopTimeout  = 2 * time.Minute
	windowsStartTimeout = 30 * time.Second
)

func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// openTerminal uses standard input when it is a console; it is not under CI,
// a service or a redirected pipe.
func openTerminal() *terminal {
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) != nil {
		return nil
	}
	return &terminal{in: os.Stdin, out: os.Stdout, close: func() error { return nil }}
}

// windowsService controls TelradRelay through the Service Control Manager and
// waits for each change to finish, unlike sc.exe, which returns while the
// service is still stopping or starting.
type windowsService struct{ name string }

func nativeService() serviceControl { return windowsService{name: windowsServiceName} }

func (service windowsService) open() (*mgr.Mgr, *mgr.Service, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the Service Control Manager: %w", err)
	}
	handle, err := manager.OpenService(service.name)
	if err != nil {
		manager.Disconnect()
		return nil, nil, err
	}
	return manager, handle, nil
}

func (service windowsService) running() (bool, error) {
	manager, handle, err := service.open()
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer manager.Disconnect()
	defer handle.Close()
	status, err := handle.Query()
	if err != nil {
		return false, err
	}
	return status.State != svc.Stopped, nil
}

func (service windowsService) start() error {
	manager, handle, err := service.open()
	if err != nil {
		return fmt.Errorf("start %s: %w", service.name, err)
	}
	defer manager.Disconnect()
	defer handle.Close()
	if err := handle.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start %s: %w", service.name, err)
	}
	return waitForServiceState(handle, svc.Running, windowsStartTimeout)
}

func (service windowsService) stop() error {
	manager, handle, err := service.open()
	if err != nil {
		return fmt.Errorf("stop %s: %w", service.name, err)
	}
	defer manager.Disconnect()
	defer handle.Close()
	return stopWindowsService(handle)
}

func (service windowsService) restart() error {
	if err := service.stop(); err != nil {
		return err
	}
	return service.start()
}

// remove stops and deletes the service. A missing service is not an error.
func (service windowsService) remove() (bool, error) {
	manager, handle, err := service.open()
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer manager.Disconnect()
	defer handle.Close()
	if err := stopWindowsService(handle); err != nil {
		return false, err
	}
	if err := handle.Delete(); err != nil {
		return false, fmt.Errorf("delete %s: %w", service.name, err)
	}
	return true, nil
}

func stopWindowsService(handle *mgr.Service) error {
	if _, err := handle.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stop %s: %w", handle.Name, err)
	}
	return waitForServiceState(handle, svc.Stopped, windowsStopTimeout)
}

func waitForServiceState(handle *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := handle.Query()
		if err != nil {
			return err
		}
		if status.State == want {
			return nil
		}
		if want == svc.Running && status.State == svc.Stopped {
			return fmt.Errorf("%s stopped while starting; see the Application event log", handle.Name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not reach the expected state within %s", handle.Name, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
