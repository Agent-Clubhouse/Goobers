package main

import (
	"os"
	"path/filepath"
)

// sourceTreeDefinitionDir preserves the flat source-tree convention and also
// accepts the nested config/ layout whose sibling goobers/ contains shared
// personas. Do not reinterpret a present but unreadable root manifest.
func sourceTreeDefinitionDir(root string) string {
	if _, err := os.Stat(filepath.Join(root, "manifest.yaml")); !os.IsNotExist(err) {
		return root
	}
	nested := filepath.Join(root, "config")
	if _, err := os.Stat(filepath.Join(nested, "manifest.yaml")); err == nil {
		return nested
	}
	return root
}
