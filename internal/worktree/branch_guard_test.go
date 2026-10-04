package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUnoccupiedBranchHoldsAbsentMirrorLockAndBoundsWait(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = m.WithUnoccupiedBranch(t.Context(), "https://example.invalid/repo.git", "fix", func(ctx context.Context) error {
		short, cancel := context.WithTimeout(ctx, 15*time.Millisecond)
		defer cancel()
		if err := m.WithUnoccupiedBranch(short, "https://example.invalid/repo.git", "fix", func(context.Context) error { t.Fatal("lock was not held"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(m.Root); err != nil || len(entries) != 0 {
		t.Fatal("guard materialized repository", entries, err)
	}
	if err = m.WithUnoccupiedBranch(t.Context(), "https://example.invalid/repo.git", "fix", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestUnoccupiedBranchRefusesRetainedPinnedCustody(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	url := "https://example.invalid/repo.git"
	if err = os.MkdirAll(filepath.Join(m.PinnedRoot(), repoKey(url), "pin"), 0700); err != nil {
		t.Fatal(err)
	}
	called := false
	err = m.WithUnoccupiedBranch(t.Context(), url, "fix", func(context.Context) error { called = true; return nil })
	if err == nil || called {
		t.Fatal(err, called)
	}
}
