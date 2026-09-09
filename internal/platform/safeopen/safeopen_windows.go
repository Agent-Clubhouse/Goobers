//go:build windows

package safeopen

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// open is the windows equivalent of unix's O_NOFOLLOW open. CreateFile with
// FILE_FLAG_OPEN_REPARSE_POINT opens a reparse point (symlink, junction, mount
// point) as the link itself rather than traversing it; we then reject any
// handle whose FILE_ATTRIBUTE_REPARSE_POINT is set, so a link is never
// followed. FILE_FLAG_BACKUP_SEMANTICS is required to obtain a handle to a
// directory (the asset root and its subdirectories) via CreateFile.
//
// Compiled but not yet exercised until the Windows CI job runs — the runtime
// half is the tracked follow-up, exactly as #623 deferred proc's Job Objects.
func open(path string) (*os.File, error) {
	return openNoFollow(path)
}

// openAt resolves name against dir's path and opens it no-follow. x/sys/windows
// exposes no fd-relative open (that needs NtCreateFile with a RootDirectory
// handle), so unlike the unix openat this keeps the atomic per-leaf no-follow
// guarantee but not the fd-relative parent-swap guarantee; closing that gap is
// tracked hardening, documented in the package doc.
func openAt(dir *os.File, name string) (*os.File, error) {
	return openNoFollow(filepath.Join(dir.Name(), name))
}

func openNoFollow(path string) (*os.File, error) {
	return openWithAccess(path, windows.GENERIC_READ, windows.OPEN_EXISTING, false)
}

func appendAt(dir *os.File, name string) (*os.File, error) {
	// Match Go's append-only access: omit FILE_WRITE_DATA so the kernel
	// appends each write without a seek/end-of-file race between writers.
	access := uint32(windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE | windows.FILE_READ_ATTRIBUTES)
	return openWithAccess(filepath.Join(dir.Name(), name), access, windows.OPEN_ALWAYS, true)
}

func openWithAccess(path string, access, disposition uint32, appendOnly bool) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		p,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		disposition,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		// windows.Errno maps ERROR_FILE_NOT_FOUND/ERROR_PATH_NOT_FOUND onto
		// fs.ErrNotExist, so the no-assets case still resolves for callers.
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("%w: %s", ErrSymlink, path)
	}
	if appendOnly && (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.NumberOfLinks != 1) {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("safeopen: append requires a regular singly-linked file")
	}
	return os.NewFile(uintptr(handle), path), nil
}
