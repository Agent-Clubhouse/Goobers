package main

import (
	"io/fs"
	"path/filepath"
)

// treeSizeAcross totals the bytes of every file named name under any of roots.
//
// Callers must pass the *run* roots, not the instance root. The instance journal
// is also called events.jsonl (scheduler/events.jsonl), so walking the instance
// root silently folds a 324 MB instance journal into the "run events" figure —
// and, worse, makes it the maximum of the per-run size distribution, which
// inverts every tail assertion built on that distribution.
func treeSizeAcross(roots []string, name string) int64 {
	var total int64
	for _, root := range roots {
		total += treeSize(root, name)
	}
	return total
}

// treeSizeUnderAcross totals the bytes beneath a directory named dir under any
// of roots.
func treeSizeUnderAcross(roots []string, dir string) int64 {
	var total int64
	for _, root := range roots {
		total += treeSizeUnder(root, dir)
	}
	return total
}

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
