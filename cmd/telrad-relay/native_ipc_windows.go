//go:build windows && !relay_container

package main

import (
	"context"
	"errors"
	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"net"
	"unsafe"
)

const managementPipe = `\\.\pipe\TelradRelay.Management`
const pipeClientAccess = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE

func requireServiceIdentity() error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\TelradRelay`)
	if err != nil {
		return err
	}
	if !user.User.Sid.Equals(sid) {
		return errors.New("managed Relay must run as NT SERVICE\\TelradRelay")
	}
	return nil
}

// The mutation pipe has an explicit Administrators/SYSTEM-only client DACL.
// Authorization is checked by Windows when the connection is established.
func managementPeerIsAdministrator(net.Conn) bool { return true }
func listenManagement() ([]managementListener, error) {
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\TelradRelay`)
	if err != nil {
		return nil, err
	}
	var listeners []managementListener
	for _, admin := range []bool{false, true} {
		suffix := ".Read"
		clients := "(A;;0x00100083;;;AU)"
		if admin {
			suffix = ".Admin"
			clients = "(A;;0x00100083;;;BA)(A;;0x00100083;;;SY)"
		}
		// Avoid GENERIC_WRITE for clients: it includes FILE_CREATE_PIPE_INSTANCE.
		listener, err := winio.ListenPipe(managementPipe+suffix, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;" + sid.String() + ")" + clients, InputBufferSize: 4096, OutputBufferSize: 4096})
		if err != nil {
			for _, l := range listeners {
				l.listener.Close()
			}
			return nil, err
		}
		listeners = append(listeners, managementListener{listener, admin})
	}
	return listeners, nil
}
func dialManagement(ctx context.Context, admin bool) (net.Conn, error) {
	suffix := ".Read"
	if admin {
		suffix = ".Admin"
	}
	c, err := winio.DialPipeAccessImpLevel(ctx, managementPipe+suffix, pipeClientAccess, winio.PipeImpLevelIdentification)
	if err != nil {
		return nil, err
	}
	fd, ok := c.(interface{ Fd() uintptr })
	if !ok {
		c.Close()
		return nil, errors.New("pipe handle unavailable")
	}
	var processID uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(fd.Fd()), &processID); err != nil {
		c.Close()
		return nil, err
	}
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		c.Close()
		return nil, err
	}
	defer windows.CloseServiceHandle(scm)
	name, _ := windows.UTF16PtrFromString(windowsServiceName)
	service, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		c.Close()
		return nil, err
	}
	defer windows.CloseServiceHandle(service)
	var state windows.SERVICE_STATUS_PROCESS
	var needed uint32
	err = windows.QueryServiceStatusEx(service, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&state)), uint32(unsafe.Sizeof(state)), &needed)
	if err != nil || state.ProcessId != processID || processID == 0 {
		c.Close()
		return nil, errors.New("Relay management server is not the installed service")
	}
	return c, nil
}
