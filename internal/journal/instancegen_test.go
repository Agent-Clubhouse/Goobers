package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstanceEventsFilenameGeneration0IsLegacyName(t *testing.T) {
	if got := instanceEventsFilename(0); got != fileEvents {
		t.Fatalf("instanceEventsFilename(0) = %q, want %q (backward compatible with pre-generation instance dirs)", got, fileEvents)
	}
}

func TestInstanceEventsFilenameNonZeroGenerationIsDistinct(t *testing.T) {
	seen := map[string]bool{instanceEventsFilename(0): true}
	for gen := 1; gen <= 3; gen++ {
		name := instanceEventsFilename(gen)
		if seen[name] {
			t.Fatalf("instanceEventsFilename(%d) = %q collides with an earlier generation", gen, name)
		}
		seen[name] = true
		if name == fileEvents {
			t.Fatalf("instanceEventsFilename(%d) reused the legacy generation-0 name", gen)
		}
	}
}

func TestResolveInstanceEventsPathFallsBackToLegacyNameWithoutPointer(t *testing.T) {
	dir := t.TempDir()
	path, gen, err := resolveInstanceEventsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 0 {
		t.Fatalf("generation = %d, want 0 for a directory with no pointer file", gen)
	}
	if path != filepath.Join(dir, fileEvents) {
		t.Fatalf("path = %q, want the legacy bare name %q", path, filepath.Join(dir, fileEvents))
	}
}

func TestResolveInstanceEventsPathFollowsPointer(t *testing.T) {
	dir := t.TempDir()
	if err := advanceInstanceEventsPointer(dir, 3); err != nil {
		t.Fatal(err)
	}
	path, gen, err := resolveInstanceEventsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 3 {
		t.Fatalf("generation = %d, want 3", gen)
	}
	want := filepath.Join(dir, instanceEventsFilename(3))
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestResolveInstanceEventsGenerationRejectsMalformedPointer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileEventsPointer), []byte("not-a-number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveInstanceEventsPath(dir); err == nil {
		t.Fatal("resolveInstanceEventsPath did not reject a malformed pointer")
	}
}

func TestCleanupStaleInstanceEventsGenerationKeepsCurrentAndPrevious(t *testing.T) {
	dir := t.TempDir()
	for gen := 0; gen <= 3; gen++ {
		if err := os.WriteFile(filepath.Join(dir, instanceEventsFilename(gen)), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the sequence of cleanup calls a run of compactions actually
	// makes: each compaction advances by exactly one generation and cleans
	// up only the one that just fell out of the "current + previous" window.
	cleanupStaleInstanceEventsGeneration(dir, 2)
	cleanupStaleInstanceEventsGeneration(dir, 3)

	for gen := 0; gen <= 1; gen++ {
		if _, err := os.Stat(filepath.Join(dir, instanceEventsFilename(gen))); !os.IsNotExist(err) {
			t.Fatalf("generation %d should have been cleaned up (currentGen=3), stat err = %v", gen, err)
		}
	}
	for gen := 2; gen <= 3; gen++ {
		if _, err := os.Stat(filepath.Join(dir, instanceEventsFilename(gen))); err != nil {
			t.Fatalf("generation %d should have survived cleanup (currentGen=3): %v", gen, err)
		}
	}
}

func TestCleanupStaleInstanceEventsGenerationIsNoOpBeforeTwoGenerationsExist(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// currentGen=0 and currentGen=1 both have no stale generation to reclaim
	// (staleGen would be negative), so neither call should touch anything.
	cleanupStaleInstanceEventsGeneration(dir, 0)
	cleanupStaleInstanceEventsGeneration(dir, 1)
	if _, err := os.Stat(filepath.Join(dir, fileEvents)); err != nil {
		t.Fatalf("generation 0 was removed too early: %v", err)
	}
}

func TestCleanupStaleInstanceEventsGenerationToleratesAlreadyMissingFile(t *testing.T) {
	dir := t.TempDir()
	// No generation files exist at all; cleanup must not panic or error.
	cleanupStaleInstanceEventsGeneration(dir, 5)
}
