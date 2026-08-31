// Package configtree identifies non-definition content within a config tree.
package configtree

import (
	"path/filepath"
	"strings"
)

// IsGaggleSkillsDir reports whether path is a gaggle-scoped skill package root.
func IsGaggleSkillsDir(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	return len(parts) == 3 && parts[0] == "gaggles" && parts[1] != "" && parts[2] == "skills"
}

// ShouldSkipConfigDirExcludingAssets reports whether a directory should be
// skipped during config tree walks, excluding asset directory checks. This is
// useful when asset validation must occur before skipping. It returns true for:
//   - Hidden directories (starting with "."), except the root itself
//   - Gaggle-scoped skill package directories
func ShouldSkipConfigDirExcludingAssets(root, path string) bool {
	// Skip hidden directories except root
	if path != root && strings.HasPrefix(filepath.Base(path), ".") {
		return true
	}
	// Skip gaggle skills directories
	if IsGaggleSkillsDir(root, path) {
		return true
	}
	return false
}
