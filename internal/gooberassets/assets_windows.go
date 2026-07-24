//go:build windows

package gooberassets

import (
	"io/fs"
	"os"
)

// applyMode applies the source bundle's read-only bit on Windows. Windows does
// not use Unix mode bits for executability; executable resolution is based on
// the file extension and PATHEXT instead.
func applyMode(path string, mode fs.FileMode) error {
	return os.Chmod(path, mode.Perm())
}
