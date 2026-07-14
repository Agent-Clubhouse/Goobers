package main

import (
	"strings"
	"testing"
)

// TestTraceRejectsTraversalRunID is #244's acceptance test for `goobers
// trace`: a raw traversal run id must be refused before it is joined onto
// RunsDir, and must not read anything outside the instance root.
func TestTraceRejectsTraversalRunID(t *testing.T) {
	root := initDemo(t)

	for _, bad := range []string{"..", "../../outside", "/etc/passwd", "a/../../outside"} {
		t.Run(bad, func(t *testing.T) {
			code, _, stderr := runArgs(t, "trace", bad, root)
			if code == 0 {
				t.Fatalf("code = 0, want non-zero for traversal run id %q", bad)
			}
			if !strings.Contains(stderr, "invalid run id") {
				t.Fatalf("stderr = %q, want \"invalid run id\"", stderr)
			}
		})
	}
}

// TestRunAbortRejectsTraversalRunID is #244's acceptance test for `goobers
// run abort`: a raw traversal run id must be refused before it is joined
// onto RunsDir, and — unlike trace — before it could ever append a terminal
// event to a journal-shaped directory outside the instance.
func TestRunAbortRejectsTraversalRunID(t *testing.T) {
	root := initDemo(t)

	for _, bad := range []string{"..", "../../outside", "/etc/passwd", "a/../../outside"} {
		t.Run(bad, func(t *testing.T) {
			code, _, stderr := runArgs(t, "run", "abort", bad, root)
			if code == 0 {
				t.Fatalf("code = 0, want non-zero for traversal run id %q", bad)
			}
			if !strings.Contains(stderr, "invalid run id") {
				t.Fatalf("stderr = %q, want \"invalid run id\"", stderr)
			}
		})
	}
}
