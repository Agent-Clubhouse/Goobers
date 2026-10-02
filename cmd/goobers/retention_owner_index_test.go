package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// countingRunOwnerIndex returns an owner index over runsDir whose directory
// reads are counted, so a test can prove the listing happens once per pass.
func countingRunOwnerIndex(runsDir string, reads *int) *runOwnerIndex {
	owners := newRunOwnerIndex(runsDir)
	owners.readDir = func(dir string) ([]os.DirEntry, error) {
		*reads++
		return os.ReadDir(dir)
	}
	return owners
}

// #6359: one pass over many legacy (markerless-owner) candidates must list
// the runs directory once, and every answer must match what a fresh,
// uncached resolution of the same candidate returns.
func TestRunOwnerIndexListsRunsDirOncePerPass(t *testing.T) {
	runsDir := t.TempDir()
	seedRunPhase(t, runsDir, "run-a", journal.PhaseCompleted)
	seedRunPhase(t, runsDir, "run-a-long", journal.PhaseFailed)
	seedRunPhase(t, runsDir, "run-b", journal.PhaseRunning)
	if err := os.WriteFile(filepath.Join(runsDir, "run-c"), nil, 0o600); err != nil {
		t.Fatal(err) // a plain file never owns a worktree
	}

	candidates := []struct{ worktreeID, ownerRunID, wantOwner string }{
		{"run-a-stage", "", "run-a"},
		{"run-a-long-stage", "", "run-a-long"}, // longest prefix wins
		{"run-a", "", "run-a"},                 // exact match
		{"run-b-x-y", "", "run-b"},
		{"run-c-stage", "", ""},  // file, not a run directory
		{"run-ab-stage", "", ""}, // prefix only at a dash boundary
		{"unrelated", "", ""},
		{"wt-hash", "run-b", "run-b"}, // stamped owner skips the scan
		{"wt-hash", "gone", "gone"},
	}

	reads := 0
	owners := countingRunOwnerIndex(runsDir, &reads)
	for _, c := range candidates {
		got, err := owners.resolveRetainedWorktreeOwner(c.worktreeID, c.ownerRunID)
		if err != nil {
			t.Fatalf("resolve %q: %v", c.worktreeID, err)
		}
		if got != c.wantOwner {
			t.Fatalf("resolve %q/%q = %q, want %q", c.worktreeID, c.ownerRunID, got, c.wantOwner)
		}

		cachedPhase, cachedFound, cachedErr := retainedWorktreePhase(owners, c.worktreeID, c.ownerRunID)
		freshPhase, freshFound, freshErr := retainedWorktreePhase(newRunOwnerIndex(runsDir), c.worktreeID, c.ownerRunID)
		if cachedPhase != freshPhase || cachedFound != freshFound || (cachedErr == nil) != (freshErr == nil) {
			t.Fatalf("phase %q: cached (%q,%v,%v) != fresh (%q,%v,%v)", c.worktreeID,
				cachedPhase, cachedFound, cachedErr, freshPhase, freshFound, freshErr)
		}
		cachedMissing, cachedErr := retainedWorktreeJournalMissing(owners, c.worktreeID, c.ownerRunID)
		freshMissing, freshErr := retainedWorktreeJournalMissing(newRunOwnerIndex(runsDir), c.worktreeID, c.ownerRunID)
		if cachedMissing != freshMissing || (cachedErr == nil) != (freshErr == nil) {
			t.Fatalf("journal missing %q: cached (%v,%v) != fresh (%v,%v)", c.worktreeID,
				cachedMissing, cachedErr, freshMissing, freshErr)
		}
	}
	if reads != 1 {
		t.Fatalf("runs directory read %d times in one pass, want 1", reads)
	}
}

// Stamped owners never need the listing, so a pass of only new-style markers
// must not read the runs directory at all.
func TestRunOwnerIndexStampedOwnerSkipsListing(t *testing.T) {
	reads := 0
	owners := countingRunOwnerIndex(t.TempDir(), &reads)
	for i := 0; i < 3; i++ {
		if _, err := owners.resolveRetainedWorktreeOwner("wt", "owner"); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 0 {
		t.Fatalf("stamped owners read the runs directory %d times, want 0", reads)
	}
}

// A missing runs directory is a valid empty listing and is cached; a failed
// listing is not, so the next candidate retries exactly as it did before the
// index existed.
func TestRunOwnerIndexCachesMissingDirButRetriesErrors(t *testing.T) {
	reads := 0
	missing := countingRunOwnerIndex(filepath.Join(t.TempDir(), "absent"), &reads)
	for i := 0; i < 3; i++ {
		owner, err := missing.resolveRetainedWorktreeOwner("run-stage", "")
		if err != nil || owner != "" {
			t.Fatalf("missing runs dir resolve = %q, %v; want \"\", nil", owner, err)
		}
	}
	if reads != 1 {
		t.Fatalf("missing runs directory read %d times, want 1", reads)
	}

	runsDir := t.TempDir()
	seedRunPhase(t, runsDir, "run", journal.PhaseCompleted)
	reads = 0
	failing := newRunOwnerIndex(runsDir)
	failing.readDir = func(dir string) ([]os.DirEntry, error) {
		reads++
		if reads == 1 {
			return nil, errors.New("transient")
		}
		return os.ReadDir(dir)
	}
	if _, err := failing.resolveRetainedWorktreeOwner("run-stage", ""); err == nil {
		t.Fatal("failed listing did not surface its error")
	}
	for i := 0; i < 2; i++ {
		owner, err := failing.resolveRetainedWorktreeOwner("run-stage", "")
		if err != nil || owner != "run" {
			t.Fatalf("retry resolve = %q, %v; want \"run\", nil", owner, err)
		}
	}
	if reads != 2 {
		t.Fatalf("runs directory read %d times, want 2 (one failure, one cached success)", reads)
	}
}

// The cache is scoped to one pass: the closure handed to a Reap pass keeps
// its snapshot, while the next pass's closure sees runs created since.
func TestWorktreeRunTerminalOwnerIndexIsPassScoped(t *testing.T) {
	runsDir := t.TempDir()
	seedRunPhase(t, runsDir, "early", journal.PhaseCompleted)

	pass := worktreeRunTerminal(runsDir)
	if terminal, err := pass("early-stage"); err != nil || !terminal {
		t.Fatalf("early-stage terminal = %v, %v; want true, nil", terminal, err)
	}
	seedRunPhase(t, runsDir, "late", journal.PhaseCompleted)
	if terminal, err := pass("late-stage"); err != nil || terminal {
		t.Fatalf("same pass saw a run created after its listing: terminal = %v, %v", terminal, err)
	}
	if terminal, err := worktreeRunTerminal(runsDir)("late-stage"); err != nil || !terminal {
		t.Fatalf("next pass late-stage terminal = %v, %v; want true, nil", terminal, err)
	}
}
