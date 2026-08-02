//go:build windows

package journal

import (
	"os"

	"golang.org/x/sys/windows"
)

// openInstanceLogAppend opens the instance journal's append handle with
// FILE_SHARE_DELETE included in its share mode. os.OpenFile's Windows
// implementation (and golang.org/x/sys/windows.Open, which mirrors it) only
// requests FILE_SHARE_READ|FILE_SHARE_WRITE, so any handle it hands back
// blocks a concurrent rename/replace of that same path with
// ERROR_ACCESS_DENIED — the exact operation in-daemon compaction
// ((*InstanceLog).Compact) performs to atomically checkpoint the journal
// while the daemon (and any other independently-opened InstanceLog handle)
// keeps appending through the swap. POSIX has no equivalent restriction —
// unlink/rename never cares who has a file open — so this is a windows-only
// concern; create mirrors reopenFile's plain O_WRONLY|O_APPEND on false, or
// OpenInstanceLog's O_CREATE|O_WRONLY|O_APPEND on true.
func openInstanceLogAppend(path string, create bool) (*os.File, error) {
	namep, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	// Mirrors the O_APPEND access-rights adjustment syscall.Open/windows.Open
	// make: drop FILE_WRITE_DATA (part of GENERIC_WRITE) so every write lands
	// at EOF, keeping FILE_APPEND_DATA plus the attribute/standard rights
	// GENERIC_WRITE otherwise carries.
	access := uint32(windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE)
	sharemode := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	createmode := uint32(windows.OPEN_EXISTING)
	if create {
		createmode = windows.OPEN_ALWAYS
	}
	handle, err := windows.CreateFile(namep, access, sharemode, nil, createmode, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
