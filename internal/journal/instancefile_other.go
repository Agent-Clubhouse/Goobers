//go:build !windows

package journal

import "os"

// openInstanceLogAppend opens the instance journal's append handle. POSIX has
// no FILE_SHARE_DELETE-style restriction — unlink/rename never cares whether
// another handle has the path open, so a plain os.OpenFile append handle
// never blocks (*InstanceLog).Compact's atomic replace the way it does on
// Windows (see instancefile_windows.go).
func openInstanceLogAppend(path string, create bool) (*os.File, error) {
	flag := os.O_WRONLY | os.O_APPEND
	if create {
		flag |= os.O_CREATE
	}
	return os.OpenFile(path, flag, 0o644)
}
