//go:build windows

package main

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"unsafe"
)

func platformManagedDirectories() (string, string) {
	data, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		panic("Windows ProgramData is unavailable")
	}
	programs, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		panic("Windows ProgramFiles is unavailable")
	}
	return filepath.Join(data, "Telrad", "Relay"), filepath.Join(programs, "Telrad Relay")
}

func openRelativeDirectory(directory *os.File, name string) (*os.File, error) {
	f, err := openRelativeWindows(directory, name, windows.FILE_GENERIC_READ, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	if err != nil {
		return nil, err
	}
	if err := validateDirectoryHandle(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func createRelativeFile(directory *os.File, name string, mode os.FileMode, target string) (*os.File, error) {
	var sd *windows.SECURITY_DESCRIPTOR
	if mode.Perm()&0077 == 0 {
		token := windows.GetCurrentProcessToken()
		user, err := token.GetTokenUser()
		if err != nil {
			return nil, err
		}
		// Install this DACL as part of creation, before another user can open the
		// temporary file. Chmod cannot restrict Windows access, even before Write.
		sddl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
		if !token.IsElevated() {
			sddl += "(A;;FA;;;" + user.User.Sid.String() + ")"
		}
		if samePath(filepath.Dir(target), filepath.Dir(nativePaths().Config)) {
			sid, _, _, err := windows.LookupSID("", `NT SERVICE\TelradRelay`)
			if err == nil {
				sddl += "(A;;0x1301bf;;;" + sid.String() + ")"
			} else if !errors.Is(err, windows.ERROR_NONE_MAPPED) {
				return nil, err
			}
		}
		sd, err = windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			return nil, err
		}
	}
	return openRelativeWindows(directory, name, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE, sd)
}

func openRelativeRegular(directory *os.File, name string) (*os.File, error) {
	return openRelativeWindows(directory, name, windows.FILE_GENERIC_READ, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
}

func openCredentialLockHandle(directory *os.File, name, target string) (*os.File, error) {
	file, err := createRelativeFile(directory, name, 0600, target)
	if err == nil {
		return file, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return openRelativeWindows(directory, name, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
}

func lockCredentialHandle(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped)
}

func unlockCredentialHandle(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

func openRelativeWindows(directory *os.File, name string, access, disposition, options uint32, sd *windows.SECURITY_DESCRIPTOR) (*os.File, error) {
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(directory.Fd()), ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	err = windows.NtCreateFile(&h, access, &oa, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		if s, ok := err.(windows.NTStatus); ok {
			err = s.Errno()
		}
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(h), name), nil
}
func validateRegularHandle(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return errors.New("Relay file must be regular with exactly one link and no reparse point")
	}
	return nil
}
func validateDirectoryHandle(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("Relay directory must not be a reparse point")
	}
	return nil
}
func syncDirectoryHandle(f *os.File) error      { return nil }
func preserveFileOwner(to, from *os.File) error { return nil }

func validateAdministratorHandle(f *os.File) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	trusted := func(s *windows.SID) bool {
		return s.IsWellKnown(windows.WinBuiltinAdministratorsSid) || s.IsWellKnown(windows.WinLocalSystemSid) || s.String() == "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !trusted(owner) {
		return errors.New("managed installation owner must be Administrators or SYSTEM; rerun a reviewed installer")
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if acl == nil {
		return errors.New("managed installation requires an explicit protected DACL")
	}
	const mutation = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES | 0x40 | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("unsupported installation access control entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask&mutation != 0 && !trusted(sid) {
			return errors.New("managed installation must not be writable by the service or ordinary users; rerun a reviewed installer")
		}
	}
	return nil
}
