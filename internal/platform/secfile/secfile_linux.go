//go:build linux

package secfile

import "golang.org/x/sys/unix"

type statfsFunc func(string, *unix.Statfs_t) error

func isReadOnlyTmpfs(path string) bool {
	return isReadOnlyTmpfsWith(path, unix.Statfs)
}

func isReadOnlyTmpfsWith(path string, statfs statfsFunc) bool {
	var fs unix.Statfs_t
	if err := statfs(path, &fs); err != nil {
		return false
	}
	return fs.Type == unix.TMPFS_MAGIC && fs.Flags&unix.ST_RDONLY != 0
}
