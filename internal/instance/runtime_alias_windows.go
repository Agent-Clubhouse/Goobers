//go:build windows

package instance

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return false, err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return false, nil
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return false, err
	}
	data := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var bytesReturned uint32
	reparseErr := windows.DeviceIoControl(
		handle,
		windows.FSCTL_GET_REPARSE_POINT,
		nil,
		0,
		&data[0],
		uint32(len(data)),
		&bytesReturned,
		nil,
	)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(reparseErr, closeErr); err != nil {
		return false, err
	}
	if bytesReturned < 4 {
		return false, fmt.Errorf("read reparse point %s: response is %d bytes", path, bytesReturned)
	}
	tag := binary.LittleEndian.Uint32(data[:4])
	return tag == windows.IO_REPARSE_TAG_MOUNT_POINT || tag == windows.IO_REPARSE_TAG_SYMLINK, nil
}

func createLegacyRuntimeAlias(legacy, scoped string) error {
	target, err := filepath.Abs(scoped)
	if err != nil {
		return err
	}
	target = filepath.Clean(target)
	substituteName, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		return err
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}

	pathBytes := 2 * (len(substituteName) + len(printName))
	if pathBytes+8 > int(^uint16(0)) {
		return fmt.Errorf("junction target path is too long: %s", target)
	}
	data := make([]byte, 16+pathBytes)
	binary.LittleEndian.PutUint32(data[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(data[4:6], uint16(8+pathBytes))
	binary.LittleEndian.PutUint16(data[8:10], 0)
	binary.LittleEndian.PutUint16(data[10:12], uint16(2*(len(substituteName)-1)))
	binary.LittleEndian.PutUint16(data[12:14], uint16(2*len(substituteName)))
	binary.LittleEndian.PutUint16(data[14:16], uint16(2*(len(printName)-1)))
	for i, value := range append(substituteName, printName...) {
		binary.LittleEndian.PutUint16(data[16+2*i:], value)
	}

	if err := os.Mkdir(legacy, 0o755); err != nil {
		return err
	}
	removeAlias := true
	defer func() {
		if removeAlias {
			_ = os.Remove(legacy)
		}
	}()

	name, err := windows.UTF16PtrFromString(legacy)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	var bytesReturned uint32
	setErr := windows.DeviceIoControl(
		handle,
		windows.FSCTL_SET_REPARSE_POINT,
		&data[0],
		uint32(len(data)),
		nil,
		0,
		&bytesReturned,
		nil,
	)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(setErr, closeErr); err != nil {
		return err
	}
	removeAlias = false
	return nil
}
