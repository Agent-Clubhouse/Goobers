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
