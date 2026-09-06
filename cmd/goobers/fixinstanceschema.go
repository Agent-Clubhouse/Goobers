package main

import (
	"io"
	"os"
	"path/filepath"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/goobers/goobers/internal/instance"
)

// runFixInstanceSchema backs `goobers fix --instance-schema`: the one-line
// remedy for #4217's load refusal, adding the schemaVersion a runners:
// inventory requires.
//
// It reads and rewrites the file textually rather than through LoadConfig on
// purpose. The file it exists to repair is one the strict loader REFUSES, so
// a load-then-marshal remedy could never run on the very input that needs it.
func runFixInstanceSchema(root string, write bool, stdout, stderr io.Writer) int {
	path := instance.NewLayout(root).ConfigFile()
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "" {
		rel = path
	}

	before, err := os.ReadFile(path)
	if err != nil {
		pf(stderr, "error: read %s: %v\n", path, err)
		return 2
	}
	result, err := instance.RemedyInstanceSchemaVersion(string(before))
	if err != nil {
		pf(stdout, "FIX %s: %v\n", rel, err)
		return 1
	}
	if !result.Changed {
		pf(stdout, "FIX %s: no change needed — %s\n", rel, result.Note)
		return 0
	}
	if write {
		// Preserve the file's existing mode: instance.yaml names credential
		// SOURCES rather than values, but an operator may still have narrowed
		// its permissions, and a remedy must not widen them.
		mode := os.FileMode(0o644)
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		}
		if err := os.WriteFile(path, []byte(result.After), mode); err != nil {
			pf(stderr, "error: write %s: %v\n", path, err)
			return 2
		}
		pf(stdout, "FIX %s: %s (written)\n", rel, result.Note)
		return 0
	}
	pf(stdout, "FIX %s: %s (dry run — pass --write to apply)\n", rel, result.Note)
	diff, diffErr := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(before)),
		B:        difflib.SplitLines(result.After),
		FromFile: "a/" + rel,
		ToFile:   "b/" + rel,
		Context:  3,
	})
	if diffErr == nil {
		pf(stdout, "%s", diff)
	}
	return 0
}
