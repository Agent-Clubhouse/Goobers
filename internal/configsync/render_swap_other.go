//go:build !linux && !darwin

package configsync

import (
	"fmt"
	"os"
	"path/filepath"
)

// swapManifestPaths emulates the Linux renameat2(RENAME_EXCHANGE) behavior for
// Windows by exchanging the identities of a pointer symlink and its output
// directory via temporary names. This preserves the migration/rollback contract
// the render pipeline relies on while the platform lacks native atomic path
// exchange.
func swapManifestPaths(first, second string) error {
	parent := filepath.Dir(first)
	if filepath.Dir(second) != parent {
		return fmt.Errorf("cannot exchange paths across directories: %q and %q", first, second)
	}
	if parent == "" {
		parent = "."
	}

	baseFirst := filepath.Base(first)
	baseSecond := filepath.Base(second)
	tmp1 := filepath.Join(parent, "-swap-"+baseFirst+"-1")
	tmp2 := filepath.Join(parent, "-swap-"+baseSecond+"-2")
	for i := 0; ; i++ {
		if _, err := os.Lstat(tmp1); err != nil && os.IsNotExist(err) {
			break
		}
		tmp1 = filepath.Join(parent, fmt.Sprintf("-swap-%s-%d", baseFirst, i+1))
		if _, err := os.Lstat(tmp2); err != nil && os.IsNotExist(err) {
			break
		}
		tmp2 = filepath.Join(parent, fmt.Sprintf("-swap-%s-%d", baseSecond, i+1))
	}

	if err := os.Rename(first, tmp1); err != nil {
		return fmt.Errorf("swap %s -> %s: %w", first, tmp1, err)
	}
	if err := os.Rename(second, tmp2); err != nil {
		_ = os.Rename(tmp1, first)
		return fmt.Errorf("swap %s -> %s: %w", second, tmp2, err)
	}
	if err := os.Rename(tmp1, second); err != nil {
		_ = os.Rename(tmp2, second)
		_ = os.Rename(tmp1, first)
		return fmt.Errorf("restore %s as %s: %w", tmp1, second, err)
	}
	if err := os.Rename(tmp2, first); err != nil {
		_ = os.Rename(second, tmp2)
		_ = os.Rename(first, tmp1)
		return fmt.Errorf("restore %s as %s: %w", tmp2, first, err)
	}
	return nil
}
