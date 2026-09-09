//go:build windows

package configmirror

import (
	"errors"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MoveFileEx replacement refuses an existing target with open handles even
// when readers permit deletion. FileRenameInfoEx's POSIX semantics preserve
// those readers while atomically assigning the name to the new generation.
// Unsupported filesystems fail closed; removing the old name first would
// introduce a missing-snapshot window and is not an acceptable fallback.
func replaceSnapshot(source, destination string) (result error) {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(absolute)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	handle, err := windows.CreateFile(sourcePath, windows.DELETE|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH, 0)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, windows.CloseHandle(handle)) }()
	var layout struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [1]uint16
	}
	offset := unsafe.Offsetof(layout.FileName)
	buffer := make([]byte, int(offset)+len(name)*2)
	info := (*struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
	})(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[offset])), len(name)), name)
	if err := windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer))); err != nil {
		return err
	}
	return windows.FlushFileBuffers(handle)
}
