//go:build windows

package journal

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// replaceFileW is the raw Win32 ReplaceFileW procedure. golang.org/x/sys/windows
// does not wrap it (unlike MoveFileEx), so it's declared directly against
// kernel32.dll.
var (
	modkernel32     = windows.NewLazySystemDLL("kernel32.dll")
	procReplaceFile = modkernel32.NewProc("ReplaceFileW")
)

// compactReplaceFile atomically replaces destination's content with source's,
// tolerating readers that already have destination open. Unlike MoveFileEx
// (what durability.ReplaceFile uses, and what os.Rename resolves to via
// syscall.Rename on Windows), which requires every open handle on the
// destination to carry FILE_SHARE_DELETE or fails with ERROR_ACCESS_DENIED,
// the Win32 ReplaceFile API is specifically designed to swap a file's content
// out from under readers that opened it the ordinary way (this is exactly
// what backup/safe-save features use it for). In-daemon compaction
// ((*InstanceLog).Compact) needs precisely this: the daemon (and any other
// independently-opened InstanceLog handle, or an unrelated reader like the
// portal/CLI) may hold an open handle on events.jsonl while a compaction
// cycle rewrites it, and none of those readers can be required to use a
// share-delete-aware open (they may be outside this package entirely).
//
// Kept local to the journal package rather than folded into
// internal/platform/durability.ReplaceFile: that helper is shared by
// state.json/run.yaml/worktree markers/agentkit's repository cache, none of
// which have this "tolerate a live reader" requirement, and widening its
// behavior for every caller is a bigger blast radius than this one compactor
// needs.
func compactReplaceFile(source, destination string) error {
	destp, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return fmt.Errorf("journal: encode destination path: %w", err)
	}
	srcp, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return fmt.Errorf("journal: encode source path: %w", err)
	}
	r1, _, callErr := procReplaceFile.Call(
		uintptr(unsafe.Pointer(destp)),
		uintptr(unsafe.Pointer(srcp)),
		0, // lpBackupFileName: no backup kept
		0, // dwReplaceFlags
		0, // lpExclude: reserved, must be NULL
		0, // lpReserved: reserved, must be NULL
	)
	if r1 == 0 {
		return fmt.Errorf("journal: ReplaceFile %s -> %s: %w", source, destination, callErr)
	}
	return nil
}
