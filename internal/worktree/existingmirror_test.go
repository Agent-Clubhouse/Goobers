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
