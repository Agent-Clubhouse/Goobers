//go:build darwin

package configsync

import "golang.org/x/sys/unix"

func swapManifestPaths(first, second string) error {
	return unix.RenamexNp(first, second, unix.RENAME_SWAP)
}
