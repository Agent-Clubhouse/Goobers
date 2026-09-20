package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWithExistingMirrorDoesNotProvisionAndHoldsRepositoryLock(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	const repository = "https://example.invalid/owner/repo.git"
	called := false
	visit := func(string) error { called = true; return nil }
	if found, err := m.WithExistingMirror(context.Background(), repository, visit); err != nil || found || called {
		t.Fatalf("missing mirror visited: found=%t called=%t err=%v", found, called, err)
	}
	dir := m.repoDirForKey(repoKey(repository))
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lookup provisioned mirror: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("visitor failed")
	found, err := m.WithExistingMirror(context.Background(), repository, func(got string) error {
		if got != dir {
			t.Fatalf("visited %q, want %q", got, dir)
		}
		if lock := m.lockFor(repoKey(repository)); lock.TryLock() {
			lock.Unlock()
			t.Fatal("visitor ran without repository lock")
		}
		return wantErr
	})
	if !found || !errors.Is(err, wantErr) {
		t.Fatalf("visitor failure lost: found=%t err=%v", found, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if found, err := m.WithExistingMirror(ctx, repository, visit); found || !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled visit: found=%t called=%t err=%v", found, called, err)
	}
}

func TestWithExistingMirrorRefusesNonDirectory(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const repository = "repo"
	path := filepath.Join(m.Root, repoKey(repository))
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := m.WithExistingMirror(context.Background(), repository, func(string) error {
		t.Fatal("visited non-directory")
		return nil
	})
	if found || err == nil {
		t.Fatalf("non-directory accepted: found=%t err=%v", found, err)
	}
}

// TestWithExistingMirrorVisitsThroughAliasedRoot pins the sibling of a live
// wedge: an instance migrated from the pre-gaggle layout keeps its
// instance-root workcopies entry as a symlink to the gaggle's own directory,
// and a pinned-project gaggle roots its manager at exactly that entry. Judging
// the root by the rule meant for the mirror directories beneath it refused
// every existing-mirror visit, which silently disabled snapshot retirement for
// the whole instance. The mirror directories themselves stay strict.
func TestWithExistingMirrorVisitsThroughAliasedRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "gaggles", "one", "workcopies")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "workcopies")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	m, err := NewManager(alias)
	if err != nil {
		t.Fatal(err)
	}
	const repository = "https://example.invalid/owner/repo.git"
	key := repoKey(repository)
	dir := m.repoDirForKey(key)
	if found, err := m.WithExistingMirror(context.Background(), repository, func(string) error {
		t.Fatal("visited absent mirror")
		return nil
	}); found || err != nil {
		t.Fatalf("absent mirror under aliased root: found=%t err=%v", found, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var visited string
	found, err := m.WithExistingMirror(context.Background(), repository, func(got string) error {
		visited = got
		return nil
	})
	if err != nil || !found || visited != dir {
		t.Fatalf("aliased root refused the visit: found=%t visited=%q want=%q err=%v", found, visited, dir, err)
	}

	// A substituted mirror directory beneath the aliased root is still refused.
	substituted := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(substituted, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(substituted, dir); err != nil {
		t.Fatal(err)
	}
	found, err = m.WithExistingMirror(context.Background(), repository, func(string) error {
		t.Fatal("visited substituted mirror")
		return nil
	})
	if found || err == nil {
		t.Fatalf("substituted mirror accepted: found=%t err=%v", found, err)
	}
}
