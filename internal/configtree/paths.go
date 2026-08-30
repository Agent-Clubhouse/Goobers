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

// SkipDir reports whether a config directory should be ignored while walking the
// config tree. This includes dotfile directories (notably .git in a tracked
// config repo) without hiding the goober-assets validation path, which callers
// that need to inspect those directories handle specifically.
func SkipDir(root, path string) bool {
	if path == root {
		return false
	}
	if strings.HasPrefix(filepath.Base(path), ".") {
		return true
	}
	return IsGaggleSkillsDir(root, path)
}
