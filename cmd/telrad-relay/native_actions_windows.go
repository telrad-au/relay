//go:build windows && !relay_container

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"io"
	"strings"
	"time"
	"unsafe"
)

func platformAdministrator() bool { return windows.GetCurrentProcessToken().IsElevated() }

const updateTransferPipe = `\\.\pipe\TelradRelay.Update.`

func launchNativeAction(args []string, payload io.Reader) error {
	if err := validateNativeAction(args); err != nil {
		return err
	}
	if platformAdministrator() {
		return executeNativeAction(args, payload)
	}
	var transferDone chan error
	if payload != nil {
		token := make([]byte, 16)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		id := hex.EncodeToString(token)
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return err
		}
		l, err := winio.ListenPipe(updateTransferPipe+id, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;" + user.User.Sid.String() + ")(A;;0x00100083;;;BA)(A;;0x00100083;;;SY)", OutputBufferSize: 65536})
		if err != nil {
			return err
		}
		defer l.Close()
		args = append(append([]string{}, args...), id)
		transferDone = make(chan error, 1)
		go func() {
			c, err := l.Accept()
			if err != nil {
				transferDone <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(2 * time.Minute))
			_, err = io.Copy(c, payload)
			transferDone <- err
		}()
	}
	err := runElevatedInstalled(args)
	if err != nil {
		return err
	}
	if transferDone != nil {
		select {
		case err := <-transferDone:
			return err
		case <-time.After(time.Second):
			return errors.New("update transfer did not finish")
		}
	}
	return nil
}

func receiveUpdateTransfer(ctx context.Context, id string) (io.Reader, func(), error) {
	if !validTransferID(id) {
		return nil, func() {}, errors.New("invalid update transfer identifier")
	}
	c, err := winio.DialPipeAccessImpLevel(ctx, updateTransferPipe+id, pipeClientAccess, winio.PipeImpLevelIdentification)
	if err != nil {
		return nil, func() {}, err
	}
	c.SetDeadline(time.Now().Add(2 * time.Minute))
	return c, func() { c.Close() }, nil
}

// SHELLEXECUTEINFOW, using SEE_MASK_NOCLOSEPROCESS to propagate cancellation
// and completion instead of reporting a successful UAC launch as completion.
type shellExecuteInfo struct {
	Size, Mask                        uint32
	Window                            windows.Handle
	Verb, File, Parameters, Directory *uint16
	Show                              int32
	Instance                          windows.Handle
	IDList                            uintptr
	Class                             *uint16
	ClassKey                          windows.Handle
	HotKey                            uint32
	Icon, Process                     windows.Handle
}

func runElevatedInstalled(args []string) error {
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(nativePaths().Executable)
	// All arguments are enumerated verbs, validated SemVer or hexadecimal IDs.
	params, _ := windows.UTF16PtrFromString("native-action " + strings.Join(args, " "))
	directory, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	cwd, _ := windows.UTF16PtrFromString(directory)
	info := shellExecuteInfo{Mask: 0x40, Verb: verb, File: file, Parameters: params, Directory: cwd, Show: 1}
	info.Size = uint32(unsafe.Sizeof(info))
	result, _, err := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW").Call(uintptr(unsafe.Pointer(&info)))
	if result == 0 {
		return fmt.Errorf("administrator action was cancelled or denied: %w", err)
	}
	defer windows.CloseHandle(info.Process)
	if _, err := windows.WaitForSingleObject(info.Process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.Process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("administrator action failed (exit %d)", code)
	}
	return nil
}

func platformWindowsSystemDirectory() string {
	path, err := windows.GetSystemDirectory()
	if err != nil {
		return ""
	}
	return path
}
