package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

// seedRecoveryDebris creates the reservation shape a crashed publish leaves
// behind: a real directory inside the inventory that holds no readable record.
// It counts toward maxSnapshots (the cap counts directory entries, not valid
// records) and neither reclaim path can retire it.
func seedRecoveryDebris(t *testing.T, layout instance.Layout, name string) string {
	t.Helper()
	dir := filepath.Join(layout.Root, "recovery", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".publish.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRecoverySweepSurvivesUnreadableReservation is the regression for the
// instance wedge behind a "recovery inventory is full: 128 of 128" stage
// failure.
//
// The chain it guards: the periodic retention sweep is the ONLY path that
// reclaims inventory capacity by policy. It read the inventory all-or-nothing,
// so ONE crashed publish — a reservation holding only lock files — failed the
// whole scan. From that moment nothing was ever retired, the inventory climbed
// to its cap, every worktree cleanup needing a recovery handoff was refused
// with ErrInventoryFull, worktree reuse stopped, and stages died at
// "create worktree: ... reconcile released branch". #5092 fixed exactly this
// shape for the eviction path and left the sweep strict.
//
// The assertion is deliberately about REACHING the per-entry work, not about
// the sweep succeeding: with no managers wired the per-entry retirement cannot
// complete here, and that is fine. What must not happen is failing at the read
// with every entry unexamined, which is what wedged the instance.
func TestRecoverySweepSurvivesUnreadableReservation(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
	now := time.Now().UTC()
	seedRecoverySelection(t, layout, repo, "healthy", "1", now.Add(-time.Hour), now.Add(time.Hour), true)
	seedRecoveryDebris(t, layout, "0000000000000000-deadbeef")

	setup := &schedulerSetup{Config: &instance.Config{}}
	err := retireExpiredRecovery(
		context.Background(), layout, setup, nil, map[string]string{}, true, io.Discard, io.Discard,
	)

	// The unreadable reservation is reported rather than silently skipped: it
	// still occupies a slot, so an operator must be able to see why capacity is
	// not coming back.
	if err == nil || !strings.Contains(err.Error(), "unreadable recovery reservation") {
		t.Fatalf("sweep error = %v, want it to name the unreadable reservation", err)
	}
	// And it must NOT be the all-or-nothing read failure that aborted before
	// examining anything.
	if strings.Contains(err.Error(), "inspect recovery reservation") {
		t.Fatalf("sweep still aborts at the strict read: %v", err)
	}
	// Proof it reached the per-entry work: the healthy entry was examined and
	// failed for its OWN reason (no managers are wired in this fixture), rather
	// than being skipped wholesale by an aborted scan. That distinction is the
	// whole regression — a wedged instance failed with every entry unexamined.
	if !strings.Contains(err.Error(), "requires owning run journal") {
		t.Fatalf("sweep did not reach the healthy entry; error = %v", err)
	}
}

// TestRecoverySweepReadsPastDebrisAtTheSeam pins the read behavior the sweep
// depends on, independently of the sweep's own wiring: the tolerant read
// returns the healthy entries AND reports the broken one, where the strict read
// returns nothing at all.
//
// Both halves matter. Tolerance without the report would turn a hard wedge into
// a silent leak — capacity quietly consumed by reservations nobody is told
// about — which is the failure mode this issue class keeps producing.
func TestRecoverySweepReadsPastDebrisAtTheSeam(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
	now := time.Now().UTC()
	seedRecoverySelection(t, layout, repo, "healthy", "1", now.Add(-time.Hour), now.Add(time.Hour), true)
	seedRecoveryDebris(t, layout, "0000000000000000-deadbeef")
	root := filepath.Join(layout.Root, "recovery")

	if _, err := recovery.ReadInventory(context.Background(), root, 128); err == nil {
		t.Fatal("strict read succeeded on debris; the wedge precondition no longer reproduces")
	}

	entries, unreadable, err := recovery.ReadInventoryTolerant(context.Background(), root, 128)
	if err != nil {
		t.Fatalf("tolerant read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("tolerant read returned %d healthy entries, want 1", len(entries))
	}
	if len(unreadable) != 1 {
		t.Fatalf("tolerant read reported %d unreadable reservations, want 1", len(unreadable))
	}
}

// TestPrioritizeAbandonedSkipsUnclassifiableEntries covers the SECOND abort
// point on the same path. Ordering abandoned entries first is a preference, so
// failing to classify one entry must not deny every other entry its retention
// policy — which is what returning an error here did.
func TestPrioritizeAbandonedSkipsUnclassifiableEntries(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
	now := time.Now().UTC()
	good := seedRecoverySelection(t, layout, repo, "good", "1", now.Add(-time.Hour), now.Add(time.Hour), true)

	entries := []recovery.InventoryEntry{
		{RecordPath: filepath.Join(layout.Root, "recovery", "missing", "record.json")},
		{RecordPath: good},
	}
	var failures error
	ordered := prioritizeAbandonedRecovery(entries, nil, &failures)

	if len(ordered) != 1 || ordered[0].RecordPath != good {
		t.Fatalf("ordered = %#v, want only the classifiable entry retained", ordered)
	}
	if failures == nil || !strings.Contains(failures.Error(), "classify recovery entry") {
		t.Fatalf("failures = %v, want the unclassifiable entry reported", failures)
	}
	if errors.Is(failures, context.Canceled) {
		t.Fatal("unexpected cancellation in failures")
	}
}
