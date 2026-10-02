package worktree

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// #6359: the markerless reaper lists a repository's worktrees once per pass.
// Only a "registered" answer — which authorizes removal on its own — is
// re-confirmed live; "not registered" comes from the snapshot because it
// routes the candidate to the stricter terminal-journal check.
func TestRegistrationSnapshotListsOncePerPassAndReconfirmsRegistered(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	registered := filepath.Join(root, "registered")
	deregistered := filepath.Join(root, "deregistered")
	unregistered := filepath.Join(root, "unregistered")

	calls := 0
	live := []registeredWorktree{{Path: registered}, {Path: deregistered}}
	snapshot := newRegistrationSnapshot(root)
	snapshot.list = func(_ context.Context, repoDir string) ([]registeredWorktree, error) {
		if repoDir != root {
			t.Fatalf("listed %q, want %q", repoDir, root)
		}
		calls++
		return append([]registeredWorktree(nil), live...), nil
	}
	ctx := context.Background()

	check := func(path string, want bool, wantCalls int) {
		t.Helper()
		got, err := snapshot.registered(ctx, path)
		if err != nil {
			t.Fatalf("registered(%s): %v", filepath.Base(path), err)
		}
		if got != want {
			t.Fatalf("registered(%s) = %v, want %v", filepath.Base(path), got, want)
		}
		if calls != wantCalls {
			t.Fatalf("after %s: %d listings, want %d", filepath.Base(path), calls, wantCalls)
		}
	}

	check(first, false, 1)        // first candidate takes the snapshot
	check(unregistered, false, 1) // answered from the snapshot
	check(first, false, 1)
	check(registered, true, 2) // positive is re-confirmed live

	// Deregistered after the snapshot: the live re-check must win, so the
	// candidate falls back to the terminal-journal check instead of being
	// reaped as an incomplete create.
	live = []registeredWorktree{{Path: registered}}
	check(deregistered, false, 3)

	// Registered after the snapshot: the snapshot's "not registered" stands,
	// which only ever makes the candidate harder to reap.
	live = append(live, registeredWorktree{Path: unregistered})
	check(unregistered, false, 3)
}

// A failed listing is not cached: the next candidate retries it, as each
// candidate did before the snapshot existed.
func TestRegistrationSnapshotRetriesFailedListing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "wt")
	calls := 0
	snapshot := newRegistrationSnapshot(root)
	snapshot.list = func(context.Context, string) ([]registeredWorktree, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient")
		}
		return []registeredWorktree{{Path: path}}, nil
	}
	if _, err := snapshot.registered(context.Background(), path); err == nil {
		t.Fatal("failed listing did not surface its error")
	}
	got, err := snapshot.registered(context.Background(), path)
	if err != nil || !got {
		t.Fatalf("retry registered = %v, %v; want true, nil", got, err)
	}
	if calls != 2 {
		t.Fatalf("%d listings, want 2", calls)
	}
}
