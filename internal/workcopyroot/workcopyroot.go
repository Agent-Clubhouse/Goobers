// Package workcopyroot holds the normalization rules the daemon applies to a
// configured managed-working-copy root, so canonical validation can reject the
// same values at the earliest boundary instead of letting `goobers validate`
// report success for a configuration that later fails daemon definition
// construction (#3663).
package workcopyroot

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Validate reports why root cannot be used as a managed working-copy root, or
// nil when it can. An empty root means "not configured" and always passes;
// field names the configuration path for the diagnostic.
func Validate(field, root string) error {
	if root == "" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("%s must be an absolute path: %q", field, root)
	}
	return nil
}

// Key normalizes path to the comparison key used when detecting cross-gaggle
// collisions: absolute, cleaned, and case-folded on Windows, whose filesystem
// paths are case-insensitive.
func Key(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	key := filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key, nil
}

// Overlap reports whether two normalized (Key) managed working-copy
// directories share mutable state: the same directory, or one nested beneath
// the other. Both are collisions, because a gaggle's mirrors and worktrees own
// their whole subtree.
func Overlap(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+string(filepath.Separator)) ||
		strings.HasPrefix(b, a+string(filepath.Separator))
}
