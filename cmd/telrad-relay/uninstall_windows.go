//go:build windows

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

// windowsUninstaller removes what packaging/install.ps1 installs.
type windowsUninstaller struct {
	installDir, companyDir, dataDir string
}

func nativeUninstaller() uninstaller {
	programFiles := os.Getenv("ProgramW6432")
	if programFiles == "" {
		programFiles = os.Getenv("ProgramFiles")
	}
	return &windowsUninstaller{
		installDir: filepath.Join(programFiles, "Telrad Relay"),
		companyDir: filepath.Join(programData(), "Telrad"),
		dataDir:    defaultDataDir(),
	}
}

func (u *windowsUninstaller) plan(purge bool) uninstallPlan {
	plan := uninstallPlan{remove: []string{
		"the " + windowsServiceName + " service and its event log source",
		"the TelradRelay-DICOM and TelradRelay-HL7 firewall rules",
		u.installDir + " (the program) and its machine PATH entry",
	}}
	data := u.dataDir + " (relay.json: the report receiver; identity.json: the pairing; accessions.ledger: the accession ledger)"
	if purge {
		plan.remove = append(plan.remove, data)
		return plan
	}
	plan.keep = []string{data}
	plan.purgeHint = fmt.Sprintf("Remove-Item -Recurse -Force '%s' (as Administrator)", u.dataDir)
	return plan
}

func (u *windowsUninstaller) remove(purge bool, out io.Writer) error {
	if _, err := (windowsService{name: windowsServiceName}).remove(); err != nil {
		return err
	}
	_ = eventlog.Remove(windowsServiceName)
	if err := removeFirewallRules(); err != nil {
		fmt.Fprintf(out, "Could not remove the firewall rules (%v); remove TelradRelay-DICOM and TelradRelay-HL7 with Remove-NetFirewallRule.\n", err)
	}
	if err := removeFromMachinePath(u.installDir); err != nil {
		fmt.Fprintf(out, "Could not remove %s from the machine PATH: %v\n", u.installDir, err)
	}
	if purge {
		if err := os.RemoveAll(u.dataDir); err != nil {
			return fmt.Errorf("remove %s: %w", u.dataDir, err)
		}
		_ = os.Remove(u.companyDir) // only when nothing else of Telrad's is there
	}
	return removeProgramDirectory(u.installDir, out)
}

func systemTool(parts ...string) string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(append([]string{root, "System32"}, parts...)...)
}

func powershellPath() string { return systemTool("WindowsPowerShell", "v1.0", "powershell.exe") }

// encodedPowerShell passes a script without any command-line quoting.
func encodedPowerShell(script string) []string {
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 0, 2*len(units))
	for _, unit := range units {
		raw = append(raw, byte(unit), byte(unit>>8))
	}
	return []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", base64.StdEncoding.EncodeToString(raw)}
}

func removeFirewallRules() error {
	script := "Remove-NetFirewallRule -Name TelradRelay-DICOM, TelradRelay-HL7 -ErrorAction SilentlyContinue"
	output, err := exec.Command(powershellPath(), encodedPowerShell(script)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// removeFromMachinePath drops directory from the machine PATH, keeping the
// value's registry type, and tells running programs the environment changed.
func removeFromMachinePath(directory string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	value, valueType, err := key.GetStringValue("Path")
	if err != nil {
		return err
	}
	var kept []string
	for _, entry := range strings.Split(value, ";") {
		if !strings.EqualFold(strings.TrimRight(entry, `\`), strings.TrimRight(directory, `\`)) {
			kept = append(kept, entry)
		}
	}
	updated := strings.Join(kept, ";")
	if updated == value {
		return nil
	}
	if valueType == registry.EXPAND_SZ {
		err = key.SetExpandStringValue("Path", updated)
	} else {
		err = key.SetStringValue("Path", updated)
	}
	if err != nil {
		return err
	}
	broadcastEnvironmentChange()
	return nil
}

func broadcastEnvironmentChange() {
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001a, 0x0002
	environment, _ := windows.UTF16PtrFromString("Environment")
	var result uintptr
	sendMessageTimeout := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	_, _, _ = sendMessageTimeout.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(environment)), smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}

// removeProgramDirectory deletes the program directory before returning.
// Windows cannot delete a running executable but can rename it within its
// volume, so when this telrad.exe lives in the directory it is first moved to
// the temporary directory; that copy is deleted once this process exits, and
// at the latest when Windows restarts. Only if the move is impossible (the
// temporary directory is on another volume) is the directory itself left for
// removal after exit.
func removeProgramDirectory(directory string, out io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		executable = ""
	}
	relative, relErr := filepath.Rel(directory, executable)
	inside := executable != "" && relErr == nil && !strings.HasPrefix(relative, "..") && !filepath.IsAbs(relative)
	if inside {
		parked := filepath.Join(os.TempDir(), fmt.Sprintf("telrad-uninstalled-%d.exe", os.Getpid()))
		if err := os.Rename(executable, parked); err != nil {
			return removeDirectoryAfterExit(directory, executable, out)
		}
		removeAfterExit(parked)
	}
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove %s: %w", directory, err)
	}
	return nil
}

// removeAfterExit deletes a file this process no longer needs but cannot
// delete while it runs: a detached PowerShell removes it after exit, and the
// file is also marked for deletion at the next restart in case that fails.
// Both are best effort; the file is a stray copy in the temporary directory.
func removeAfterExit(path string) {
	_ = startRemover(path)
	if name, err := windows.UTF16PtrFromString(path); err == nil {
		_ = windows.MoveFileEx(name, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
	}
}

// removeDirectoryAfterExit is the fallback when telrad.exe cannot leave the
// directory: everything else goes now and the rest after exit or at restart.
func removeDirectoryAfterExit(directory, executable string, out io.Writer) error {
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if strings.EqualFold(path, executable) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	if err := startRemover(directory); err == nil {
		fmt.Fprintf(out, "%s is removed shortly after this command exits.\n", directory)
		return nil
	}
	for _, path := range []string{executable, directory} {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		if err := windows.MoveFileEx(name, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); err != nil {
			return fmt.Errorf("schedule removal of %s: %w", path, err)
		}
	}
	fmt.Fprintf(out, "%s is removed when Windows next restarts.\n", directory)
	return nil
}

// startRemover starts a detached PowerShell that waits for this process to
// exit and then deletes path, retrying while it is still locked.
func startRemover(path string) error {
	literal := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	script := "Wait-Process -Id " + strconv.Itoa(os.Getpid()) + " -Timeout 300 -ErrorAction SilentlyContinue\n" +
		"for ($attempt = 0; $attempt -lt 30 -and (Test-Path -LiteralPath " + literal + "); $attempt++) {\n" +
		"  Remove-Item -LiteralPath " + literal + " -Recurse -Force -ErrorAction SilentlyContinue\n" +
		"  if (Test-Path -LiteralPath " + literal + ") { Start-Sleep -Seconds 1 }\n" +
		"}\n"
	command := exec.Command(powershellPath(), append([]string{"-WindowStyle", "Hidden"}, encodedPowerShell(script)...)...)
	// Run from outside the program directory so the helper never holds it open.
	command.Dir = systemTool()
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func sameDirectory(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

// manualRemoveCommand is the PowerShell command that deletes directory.
func manualRemoveCommand(directory string) string {
	return "Remove-Item -Recurse -Force '" + strings.ReplaceAll(directory, "'", "''") + "' (as Administrator)"
}
