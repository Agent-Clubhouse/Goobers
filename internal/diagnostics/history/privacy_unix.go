//go:build !windows

package history

import "os"

func protectHistoryDirectory(dir string, _ os.FileInfo) error {
	return os.Chmod(dir, 0o700)
}
func protectHistoryPath(_ *os.Root, _, _ string, _ bool) error {
	// New files already use 0600. Do not mutate pre-existing file aliases.
	return nil
}
