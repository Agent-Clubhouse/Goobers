//go:build !windows

package history

import "os"

func protectHistoryPath(root *os.Root, _ string, name string, directory bool) error {
	if directory {
		return root.Chmod(name, 0o700)
	}
	// New files already use 0600. Do not mutate pre-existing file aliases.
	return nil
}
