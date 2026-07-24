package worktree

import (
	"context"
	"os"
	"path/filepath"
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

// TestManagerCreateIgnoresPlantedMirrorHook is the daemon-side neutralization
// guard for the S3/#166 sandbox-escape class: an attacker who managed to
// write <mirror>/hooks/post-checkout (the mirror is shared by every run of a
// gaggle) must not gain code execution when the daemon later provisions
// ANOTHER run's worktree from the same mirror — the checkout inside
// `git worktree add` runs post-checkout, unconfined, as the daemon.
// hardenedGitArgs forces core.hooksPath to the null device on every package
// git invocation, so the planted hook must stay inert.
func TestManagerCreateIgnoresPlantedMirrorHook(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-1", BaseRef: "main", Branch: "goobers/impl/run-1",
	})
	if err != nil {
		t.Fatalf("Create run-1: %v", err)
	}
	if err := first.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("Remove run-1: %v", err)
	}

	mirror := m.repoDirForKey(repoKey(repo))
	canary := filepath.Join(t.TempDir(), "escaped")
	hook := "#!/bin/sh\n: > \"" + filepath.ToSlash(canary) + "\"\n"
	if err := os.MkdirAll(filepath.Join(mirror, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	second, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-2", BaseRef: "main", Branch: "goobers/impl/run-2",
	})
	if err != nil {
		t.Fatalf("Create run-2 with planted hook: %v", err)
	}
	if err := second.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("Remove run-2: %v", err)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("planted post-checkout hook executed during daemon-side provisioning (canary stat err = %v)", err)
	}
}
