//go:build windows && !relay_container

package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"
)

func prepareNativeInstallation() error {
	data, programs := platformManagedDirectories()
	for _, path := range []string{programs, filepath.Dir(data), data} {
		parent, err := openSafeDirectory(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = parent.root.Mkdir(filepath.Base(path), 0700)
		parent.Close()
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		d, err := openSafeDirectory(path)
		if err != nil {
			return err
		}
		// Existing service data is intentionally writable by its virtual account.
		if path != data {
			err = validateAdministratorHandle(d.file)
		}
		d.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func nativeServiceRunning() (bool, error) {
	scm, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer scm.Disconnect()
	service, err := scm.OpenService(windowsServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer service.Close()
	status, err := service.Query()
	return status.State == svc.Running, err
}
func nativeServiceFiles() []string { return nil }
func reloadNativeService() error   { return nil }

func setNativeACL(path string, serviceWrite, publicRead bool) error {
	parent, err := openSafeDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	name, err := windows.NewNTUnicodeString(filepath.Base(path))
	if err != nil {
		return err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.file.Fd()), ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var handle windows.Handle
	// MAXIMUM_ALLOWED deliberately disables SetSecurityInfo propagation to existing
	// children. Only the explicit managed leaves below have their ACLs repaired.
	// https://learn.microsoft.com/windows/win32/api/aclapi/nf-aclapi-setsecurityinfo
	err = windows.NtCreateFile(&handle, windows.MAXIMUM_ALLOWED, &oa, &windows.IO_STATUS_BLOCK{}, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 && info.NumberOfLinks != 1 {
		return errors.New("unsafe Relay ACL target")
	}
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\TelradRelay`)
	if err != nil {
		return err
	}
	access := "GRGX"
	if serviceWrite {
		access = "0x1301bf"
	}
	sddl := "O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;" + access + ";;;" + sid.String() + ")"
	if publicRead {
		sddl += "(A;OICI;GRGX;;;BU)"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, acl, nil)
}
func secureNativeState() error {
	// The service SID exists once SCM has a service record, even before startup.
	if err := ensureWindowsService(); err != nil {
		return err
	}
	paths := nativePaths()
	for _, path := range []string{filepath.Dir(paths.Executable), filepath.Dir(filepath.Dir(paths.Config))} {
		if err := setNativeACL(path, false, true); err != nil {
			return err
		}
	}
	for _, path := range []string{paths.Executable, paths.Trust, paths.Installation} {
		if err := setNativeACL(path, false, true); err != nil {
			return err
		}
	}
	if err := setNativeACL(filepath.Dir(paths.Config), true, false); err != nil {
		return err
	}
	for _, name := range []string{"relay.json", "relay-credential.json", "runtime-status.json", "relay.json.pairing-transaction.json", "relay.json.next", "relay.json.previous", "relay-credential.json.next", "relay-credential.json.previous"} {
		path := filepath.Join(filepath.Dir(paths.Config), name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := setNativeACL(path, true, false); err != nil {
			return err
		}
	}
	return nil
}
func ensureWindowsService() error {
	scm, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer scm.Disconnect()
	service, err := scm.OpenService(windowsServiceName)
	configuration := mgr.Config{DisplayName: "Telrad Relay", StartType: mgr.StartAutomatic, ServiceStartName: `NT SERVICE\TelradRelay`, SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED}
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		service, err = scm.CreateService(windowsServiceName, nativePaths().Executable, configuration, "run")
		if err != nil {
			return err
		}
		defer service.Close()
		return service.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 5 * time.Second}}, 86400)
	}
	if err != nil {
		return err
	}
	defer service.Close()
	previous, err := service.Config()
	if err != nil {
		return err
	}
	previous.BinaryPathName = `"` + nativePaths().Executable + `" run`
	previous.ServiceStartName = configuration.ServiceStartName
	previous.SidType = configuration.SidType
	return service.UpdateConfig(previous)
}

const windowsIntegrationScript = `
$ErrorActionPreference='Stop'
$target=Join-Path ([Environment]::GetFolderPath('ProgramFiles')) 'Telrad Relay'
$path=[Environment]::GetEnvironmentVariable('Path','Machine')
$rules=@(
 @{Key='Dicom';Name='TelradRelay-DICOM';Display='Telrad Relay DICOM';Port=11112},
 @{Key='HL7';Name='TelradRelay-HL7';Display='Telrad Relay HL7';Port=2575}
)
if($env:TELRAD_INSTALL_MODE -eq 'snapshot') {
 $existing=@(Get-NetFirewallRule -PolicyStore PersistentStore)
 @{Dicom=[bool]($existing | Where-Object {$_.DisplayName -eq 'Telrad Relay DICOM' -or $_.Name -eq 'TelradRelay-DICOM'});HL7=[bool]($existing | Where-Object {$_.DisplayName -eq 'Telrad Relay HL7' -or $_.Name -eq 'TelradRelay-HL7'});Path=[bool](($path -split ';') -contains $target)} | ConvertTo-Json -Compress
} elseif($env:TELRAD_INSTALL_MODE -eq 'configure') {
 $existing=@(Get-NetFirewallRule -PolicyStore PersistentStore)
 foreach($rule in $rules) {
  if(-not ($existing | Where-Object {$_.DisplayName -eq $rule.Display -or $_.Name -eq $rule.Name})) {
   New-NetFirewallRule -Name $rule.Name -DisplayName $rule.Display -Direction Inbound -Action Allow -Protocol TCP -LocalPort $rule.Port -RemoteAddress $env:TELRAD_INSTALL_FIREWALL_SCOPE -Profile Domain,Private | Out-Null
  }
 }
 if(($path -split ';') -notcontains $target) {[Environment]::SetEnvironmentVariable('Path',($path.TrimEnd(';')+';'+$target),'Machine')}
} elseif($env:TELRAD_INSTALL_MODE -eq 'restore') {
 $previous=$env:TELRAD_INSTALL_PREVIOUS | ConvertFrom-Json
 $existing=@(Get-NetFirewallRule -PolicyStore PersistentStore)
 foreach($rule in $rules) {
  if(-not $previous.($rule.Key)) {
   $existing | Where-Object Name -eq $rule.Name | Remove-NetFirewallRule
  }
 }
 if(-not $previous.Path) {[Environment]::SetEnvironmentVariable('Path',(($path -split ';' | Where-Object {$_ -ne $target}) -join ';'),'Machine')}
} else {throw 'Invalid installation operation.'}
`

func windowsIntegration(mode, remote string, previous []byte) ([]byte, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	words := utf16.Encode([]rune(windowsIntegrationScript))
	encoded := make([]byte, len(words)*2)
	for i, v := range words {
		binary.LittleEndian.PutUint16(encoded[2*i:], v)
	}
	command := exec.Command(filepath.Join(system, `WindowsPowerShell\v1.0\powershell.exe`), "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded))
	command.Env = append(serviceCommandEnvironment(), "TELRAD_INSTALL_MODE="+mode, "TELRAD_INSTALL_FIREWALL_SCOPE="+remote, "TELRAD_INSTALL_PREVIOUS="+string(previous))
	command.Stderr = os.Stderr
	return command.Output()
}

func snapshotNativeSystem() (func() error, error) {
	scm, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer scm.Disconnect()
	service, err := scm.OpenService(windowsServiceName)
	var previous *mgr.Config
	if err == nil {
		configuration, err := service.Config()
		service.Close()
		if err != nil {
			return nil, err
		}
		// Passwords cannot be recovered from SCM. Supported managed accounts do not
		// require one, so rollback never has to guess or retain a secret.
		switch strings.ToLower(configuration.ServiceStartName) {
		case `nt service\telradrelay`, "localsystem", `nt authority\localservice`, `nt authority\networkservice`:
		default:
			return nil, errors.New("custom Windows service account requires administrator repair")
		}
		previous = &configuration
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, err
	}
	eventKey, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\EventLog\Application\TelradRelay`, registry.QUERY_VALUE)
	hadEventSource := err == nil
	if err == nil {
		eventKey.Close()
	} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, err
	}
	integration, err := windowsIntegration("snapshot", "", nil)
	if err != nil {
		return nil, err
	}
	var state struct{ Dicom, HL7, Path bool }
	if err := strictJSON(integration, &state); err != nil {
		return nil, err
	}
	integration, _ = json.Marshal(state)
	return func() error {
		_, result := windowsIntegration("restore", "", integration)
		if !hadEventSource {
			err := eventlog.Remove(windowsServiceName)
			if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
				result = errors.Join(result, err)
			}
		}
		scm, err := mgr.Connect()
		if err != nil {
			return errors.Join(result, err)
		}
		defer scm.Disconnect()
		service, err := scm.OpenService(windowsServiceName)
		if previous == nil && errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return result
		}
		if err != nil {
			return errors.Join(result, err)
		}
		defer service.Close()
		if previous == nil {
			return errors.Join(result, service.Delete())
		}
		return errors.Join(result, service.UpdateConfig(*previous))
	}, nil
}

func configureNativeService(remote string) error {
	if err := eventlog.InstallAsEventCreate(windowsServiceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		log, openErr := eventlog.Open(windowsServiceName)
		if openErr != nil {
			return err
		}
		log.Close()
	}
	if remote == "" {
		remote = "LocalSubnet"
	}
	_, err := windowsIntegration("configure", remote, nil)
	return err
}
func platformSystemctlPath() string { return "" }
func serviceCommandEnvironment() []string {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil
	}
	return []string{"SystemRoot=" + filepath.Dir(system), "PATH=" + system}
}
