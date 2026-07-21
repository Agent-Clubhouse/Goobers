//go:build unix

package secfile

import (
	"fmt"
	"os"
)

// verifyPrivate rejects the file unless its permission bits deny all group and
// other access (mode & 0o077 == 0), i.e. 0600 or tighter. A stat failure is
// fail-closed: if we cannot read the mode, we cannot prove the file is private,
// so the secret is refused rather than trusted.
func verifyPrivate(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %s: cannot stat file to check permissions: %w", ErrNotPrivate, path, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s is accessible to group/other (mode %#o); run: chmod 600 %s",
			ErrNotPrivate, path, perm, path)
	}
	return nil
}
