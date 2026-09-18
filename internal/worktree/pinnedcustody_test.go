package worktree

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPinnedCustodyRejectsInvalidMetadataBeforeHandoff(t *testing.T) {
	valid := marker{RunID: "owner", OwnerRunID: "owner", RepositoryDigest: RepositoryDigest("repo"), CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"valid", "oversize", "truncated", "empty", "foreign", "directory"} {
		t.Run(mode, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := repoKey("repo")
			root := filepath.Join(m.pinnedRoot, key)
			if err := os.MkdirAll(filepath.Join(root, "pin"), 0o700); err != nil {
				t.Fatal(err)
			}
			body := string(data)
			switch mode {
			case "oversize":
				body += strings.Repeat(" ", 8193)
			case "truncated":
				body = body[:len(body)-1]
			case "empty":
				body = "{}"
			case "foreign":
				body = strings.Replace(body, `"owner_run_id":"owner"`, `"owner_run_id":"other"`, 1)
			}
			path := filepath.Join(root, pinnedCustodyFile)
			if mode == "directory" {
				err = os.Mkdir(path, 0o700)
			} else {
				err = os.WriteFile(path, []byte(body), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			called := false
			if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { called = true; return nil }); err != nil {
				t.Fatal(err)
			}
			err = m.handoffPinnedState(context.Background(), key, "owner")
			if want := mode == "valid"; called != want || (err == nil) != want {
				t.Fatalf("handoff called=%t error=%v", called, err)
			}
		})
	}
}

func TestWithPinnedWorkspaceOwnedBy(t *testing.T) {
	manager, err := NewManager(t.TempDir(), WithPinnedRoot(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	repo := newSourceRepo(t)
	const branch = "goobers/implementation/owner"
	lease, err := manager.AcquirePinned(t.Context(), PinnedOptions{
		RepoURL: repo, RunID: "owner", BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	called := false
	found, err := manager.WithPinnedWorkspaceOwnedBy(t.Context(), repo, "owner", branch, func(path string) error {
		called = true
		if path != lease.Worktree.Path {
			t.Fatalf("visited path = %q, want %q", path, lease.Worktree.Path)
		}
		return nil
	})
	if err != nil || !found || !called {
		t.Fatalf("owner lookup = %t, called=%t, %v", found, called, err)
	}
	if found, err := manager.WithPinnedWorkspaceOwnedBy(t.Context(), repo, "other", branch, func(string) error {
		t.Fatal("unexpected foreign visit")
		return nil
	}); err != nil || found {
		t.Fatalf("other lookup = %t, %v", found, err)
	}
}
