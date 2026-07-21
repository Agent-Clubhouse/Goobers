package safeopen

import (
	"errors"
	"os"
)

// ErrSymlink is returned when the opened component is a symlink (unix) or
// reparse point (windows), which safeopen refuses to follow. Callers match it
// with errors.Is and attach their own context.
var ErrSymlink = errors.New("safeopen: refusing to follow a symlink")

// Open opens path read-only without following a symlink at its final component.
// The returned *os.File is owned by the caller, who must Close it. A symlink at
// the final component yields an error satisfying errors.Is(err, ErrSymlink); a
// missing path yields one satisfying errors.Is(err, fs.ErrNotExist), so callers
// can distinguish the two exactly as a plain open would.
func Open(path string) (*os.File, error) {
	return open(path)
}

// OpenAt opens name relative to the already-open directory dir, with the same
// no-follow guarantee as Open. On unix this is a true fd-relative openat off
// dir's descriptor; see the package doc for the windows resolution difference.
func OpenAt(dir *os.File, name string) (*os.File, error) {
	return openAt(dir, name)
}
