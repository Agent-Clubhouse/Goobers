package main

import (
	"io/fs"
	"path/filepath"
)

// treeSize totals the bytes of every file named name under root.
//
// It exists so the report can state the run-events and span footprints
// separately rather than as one directory total. That split is the entire
// premise of design §5.1's two-store decision — 191 MB of events against
// 2,263 MB of spans, ~12× — so a harness that cannot report the ratio cannot
// show whether a corpus reproduces the instance it claims to model, and cannot
// measure what splitting the stores buys.
func treeSize(root, name string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is a pathology, not a failure to report
		}
		if d.IsDir() || d.Name() != name {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// treeSizeUnder totals the bytes of every file at any depth beneath a directory
// named dir under root.
//
// Depth matters: spans are content-addressed, so they land at
// `<run>/spans/sha256/<aa>/<rest>` rather than directly in `spans/`. Checking
// only the immediate parent finds nothing and silently reports a 0 B span
// footprint — which reads as "this corpus has no spans" when it has thousands.
func treeSizeUnder(root, dir string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !hasPathComponent(path, dir) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// hasPathComponent reports whether any directory component of path equals name.
func hasPathComponent(path, name string) bool {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == name {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
	}
}
