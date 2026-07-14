package worktree

import (
	"context"
	"os"
	"testing"
)

// TestManagerCreateRejectsTraversalRunID is #244's table-driven acceptance
// test: opts.RunID is joined into this worktree's path and marker key, so a
// traversal id must be refused before ANY of that happens — including
// before the managed working copy is even cloned — leaving the manager's
// root untouched.
func TestManagerCreateRejectsTraversalRunID(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)

	for _, bad := range []string{".", "..", "../../etc", "/abs", "/etc/passwd", "a/b", "a/../../b"} {
		t.Run(bad, func(t *testing.T) {
			root := t.TempDir()
			m, err := NewManager(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: bad, BaseRef: "main"}); err == nil {
				t.Fatalf("Create(RunID=%q) unexpectedly succeeded", bad)
			}
			entries, rerr := os.ReadDir(root)
			if rerr != nil {
				t.Fatalf("ReadDir: %v", rerr)
			}
			if len(entries) != 0 {
				t.Fatalf("Create(RunID=%q) touched the manager root before validating: %v", bad, entries)
			}
		})
	}
}
