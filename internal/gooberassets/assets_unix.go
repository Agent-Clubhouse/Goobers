//go:build !windows

package gooberassets

import (
	"io/fs"
	"os"
)

func applyMode(path string, mode fs.FileMode) error {
	return os.Chmod(path, mode.Perm())
}
