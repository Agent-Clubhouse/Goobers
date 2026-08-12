//go:build windows

package instance

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func isLegacyRuntimeAlias(path string, info fs.FileInfo) (bool, error) {
	if info.Mode()&fs.ModeSymlink != 0 {
		return true, nil
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(
		name,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return false, err
	}
	var data windows.ByHandleFileInformation
	infoErr := windows.GetFileInformationByHandle(handle, &data)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(infoErr, closeErr); err != nil {
		return false, err
	}
	return data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

func createLegacyRuntimeAlias(legacy, scoped string) error {
	target, err := filepath.Abs(scoped)
	if err != nil {
		return err
	}
	output, err := exec.Command("cmd", "/c", "mklink", "/J", legacy, filepath.Clean(target)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
