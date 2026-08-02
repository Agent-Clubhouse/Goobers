//go:build !windows

package journal

import "os"

// compactReplaceFile atomically replaces destination's content with source's.
// POSIX rename has no equivalent to Windows' handle-sharing restriction — an
// open reader keeps reading the old (now-unlinked) inode until it reopens the
// path — so a plain rename suffices; see compactreplace_windows.go for why
// Windows needs a different primitive.
func compactReplaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
