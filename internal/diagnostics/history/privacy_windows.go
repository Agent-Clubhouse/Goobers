//go:build windows

package history

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/windows"

	"github.com/goobers/goobers/internal/platform/secfile"
)

func protectHistoryPath(root *os.Root, dir, name string, directory bool) error {
	before, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || directory != before.IsDir() || !directory && !before.Mode().IsRegular() {
		return errors.New("unsafe diagnostic privacy target")
	}
	path := filepath.Join(dir, name)
	file, err := openPrivacyTarget(path, before, directory)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := applyHistoryPrivacy(file, path, directory); err != nil {
		return err
	}
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(before, after) {
		return errors.New("diagnostic privacy target changed")
	}
	return nil
}

// Prepare before os.OpenRoot: its Windows directory handle does not share
// DELETE, which conflicts with the MAXIMUM_ALLOWED security handle.
func protectHistoryDirectory(dir string, before os.FileInfo) error {
	file, err := openPrivacyTarget(dir, before, true)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return applyHistoryPrivacy(file, dir, true)
}

func applyHistoryPrivacy(file *os.File, path string, directory bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;" + user.User.Sid.String() + ")(A;" + flags + ";FA;;;SY)(A;" + flags + ";FA;;;BA)")
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(sd)
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return err
	}
	if err := secfile.VerifyPrivate(path); err != nil {
		return err
	}
	return nil
}

func openPrivacyTarget(path string, before os.FileInfo, directory bool) (*os.File, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// MAXIMUM_ALLOWED suppresses recursive propagation by SetSecurityInfo. Only
	// this bounded target changes; future files inherit the directory's ACEs.
	// https://learn.microsoft.com/en-us/windows/win32/api/aclapi/nf-aclapi-setsecurityinfo
	handle, err := windows.CreateFile(encoded, windows.MAXIMUM_ALLOWED, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || directory != (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) || !directory && info.NumberOfLinks != 1 {
		_ = file.Close()
		return nil, errors.New("diagnostic privacy requires an ordinary single-link target")
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, errors.New("diagnostic privacy target identity changed")
	}
	return file, nil
}
