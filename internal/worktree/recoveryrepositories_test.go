package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/platform/lock"
)

func TestRecoveryRepositoriesLocksCompleteSet(t *testing.T) {
	m, err := NewManager(t.TempDir(), WithPinnedRoot(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	const repo = "https://example.invalid/owner/repo.git"
	key := repoKey(repo)
	mirror := m.repoDirForKey(key)
	pin := filepath.Join(m.pinnedRoot, key, "pin")
	for _, path := range []string{mirror, pin} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lockPath := filepath.Join(m.pinnedRoot, key, "pin.lock")
	held, err := lock.TryAcquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	visitor := func(paths []string) error {
		called = true
		if !slices.Equal(paths, []string{mirror, pin}) {
			t.Fatalf("incomplete repositories: %v", paths)
		}
		if guard := m.lockFor(key); guard.TryLock() {
			guard.Unlock()
			t.Fatal("mirror not locked")
		}
		other, err := lock.TryAcquireExisting(lockPath)
		if other != nil {
			_ = other.Release()
			t.Fatal("pinned clone not locked")
		}
		if !errors.Is(err, lock.ErrHeld) {
			t.Fatalf("pin lock: %v", err)
		}
		return nil
	}
	if found, err := m.WithRecoveryRepositories(context.Background(), repo, visitor); found || !errors.Is(err, lock.ErrHeld) || called {
		t.Fatalf("visited partial busy set: %t %v", found, err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if found, err := m.WithRecoveryRepositories(context.Background(), repo, visitor); !found || err != nil || !called {
		t.Fatalf("visit: %t %v", found, err)
	}
}

func TestRecoveryRepositoriesDoesNotCreateOrFollowPin(t *testing.T) {
	m, err := NewManager(t.TempDir(), WithPinnedRoot(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	const repo = "repo"
	visitor := func([]string) error { t.Fatal("unexpected repository visit"); return nil }
	if found, err := m.WithRecoveryRepositories(context.Background(), repo, visitor); found || err != nil {
		t.Fatalf("absent: %t %v", found, err)
	}
	root := filepath.Join(m.pinnedRoot, repoKey(repo))
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("lookup provisioned pin: %v", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pin"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := m.WithRecoveryRepositories(context.Background(), repo, visitor); found || err == nil {
		t.Fatalf("non-directory accepted: %t %v", found, err)
	}
}

// TestRecoveryRepositoriesVisitsThroughAliasedPinnedRoot pins the fix for a
// live wedge: an instance migrated from the pre-gaggle layout keeps its
// instance-root workcopies entry as an alias to the gaggle's own directory, so
// the node-wide pinned root the daemon hands this manager is a symlink. Judging
// that alias by the rule meant for the custody directories refused every
// recovery repository visit, which silently disabled snapshot retirement for
// the whole instance.
func TestRecoveryRepositoriesVisitsThroughAliasedPinnedRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "gaggles", "one", "workcopies")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "workcopies")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m, err := NewManager(t.TempDir(), WithPinnedRoot(alias))
	if err != nil {
		t.Fatal(err)
	}
	const repo = "https://example.invalid/owner/repo.git"
	key := repoKey(repo)
	mirror := m.repoDirForKey(key)
	pin := filepath.Join(alias, key, "pin")
	for _, path := range []string{mirror, pin} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	held, err := lock.TryAcquire(filepath.Join(alias, key, "pin.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	var visited []string
	found, err := m.WithRecoveryRepositories(context.Background(), repo, func(paths []string) error {
		visited = slices.Clone(paths)
		return nil
	})
	if err != nil || !found {
		t.Fatalf("aliased pinned root refused the visit: %t %v", found, err)
	}
	if !slices.Equal(visited, []string{mirror, pin}) {
		t.Fatalf("incomplete repositories through the alias: %v", visited)
	}
}
