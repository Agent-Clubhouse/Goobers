//go:build linux

package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

func smallTmpfs(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return err
	}
	if stat.Type != unix.TMPFS_MAGIC || stat.Bsize <= 0 || stat.Blocks == 0 || stat.Blocks > diskFullLimit/uint64(stat.Bsize) {
		return errors.New("disk-full injection requires a dedicated tmpfs no larger than 64MiB")
	}
	return nil
}
