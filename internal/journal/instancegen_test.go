package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestCleanupStaleInstanceEventsGenerationsKeepsCurrentAndPrevious(t *testing.T) {
	dir := t.TempDir()
	for gen := 0; gen <= 3; gen++ {
		if err := os.WriteFile(filepath.Join(dir, instanceEventsFilename(gen)), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := cleanupStaleInstanceEventsGenerations(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 (generations 0 and 1)", removed)
	}
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

func TestCleanupStaleInstanceEventsGenerationsSweepsGenerationsStrandedByEarlierFailures(t *testing.T) {
	dir := t.TempDir()
	// Generations 0-2 were stranded by past cleanup failures; only 5 and 6
	// are inside the keep window once the pointer reaches generation 6.
	for _, gen := range []int{0, 1, 2, 5, 6} {
		if err := os.WriteFile(filepath.Join(dir, instanceEventsFilename(gen)), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := cleanupStaleInstanceEventsGenerations(dir, 6)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3 stranded generations swept in one pass", removed)
	}
	for _, gen := range []int{0, 1, 2} {
		if _, err := os.Stat(filepath.Join(dir, instanceEventsFilename(gen))); !os.IsNotExist(err) {
			t.Fatalf("stranded generation %d survived the sweep, stat err = %v", gen, err)
		}
	}
	for _, gen := range []int{5, 6} {
		if _, err := os.Stat(filepath.Join(dir, instanceEventsFilename(gen))); err != nil {
			t.Fatalf("generation %d should have survived cleanup (currentGen=6): %v", gen, err)
		}
	}
}

func TestCleanupStaleInstanceEventsGenerationsSurfacesRemovalFailure(t *testing.T) {
	dir := t.TempDir()
	for _, gen := range []int{0, 3, 4} {
		if err := os.WriteFile(filepath.Join(dir, instanceEventsFilename(gen)), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A non-empty directory where a stale generation file belongs is an
	// unremovable stand-in for the real-world sharing/permission failure,
	// and fails os.Remove identically on every platform.
	blocked := filepath.Join(dir, instanceEventsFilename(1))
	if err := os.MkdirAll(filepath.Join(blocked, "held"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := cleanupStaleInstanceEventsGenerations(dir, 4)
	if err == nil {
		t.Fatal("cleanupStaleInstanceEventsGenerations swallowed a removal failure")
	}
	if !strings.Contains(err.Error(), instanceEventsFilename(1)) {
		t.Fatalf("error %q does not name the generation that could not be removed", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error %q does not name the instance directory", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1: a failure must not stop the rest of the sweep", removed)
	}
	if _, statErr := os.Stat(filepath.Join(dir, instanceEventsFilename(0))); !os.IsNotExist(statErr) {
		t.Fatalf("generation 0 should still have been removed despite the sibling failure, stat err = %v", statErr)
	}
}

func TestCleanupStaleInstanceEventsGenerationsIsNoOpBeforeTwoGenerationsExist(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// currentGen=0 and currentGen=1 both have no stale generation to reclaim,
	// so neither call should touch anything.
	for _, gen := range []int{0, 1} {
		removed, err := cleanupStaleInstanceEventsGenerations(dir, gen)
		if err != nil {
			t.Fatalf("currentGen=%d: %v", gen, err)
		}
		if removed != 0 {
			t.Fatalf("currentGen=%d removed %d generation(s), want 0", gen, removed)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, fileEvents)); err != nil {
		t.Fatalf("generation 0 was removed too early: %v", err)
	}
}

func TestCleanupStaleInstanceEventsGenerationsToleratesAlreadyMissingFiles(t *testing.T) {
	dir := t.TempDir()
	// No generation files exist at all; cleanup must not panic or error.
	removed, err := cleanupStaleInstanceEventsGenerations(dir, 5)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
}

func TestCleanupStaleInstanceEventsGenerationsIgnoresNonGenerationFiles(t *testing.T) {
	dir := t.TempDir()
	keep := []string{
		fileEventsPointer,
		fileEvents + ".gen-bogus",
		fileEvents + ".gen-1", // non-canonical padding is not a generation file
		"unrelated.jsonl",
	}
	for _, name := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cleanupStaleInstanceEventsGenerations(dir, 9); err != nil {
		t.Fatal(err)
	}
	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%q was removed by the generation sweep: %v", name, err)
		}
	}
}

func TestParseInstanceEventsGenerationRoundTrips(t *testing.T) {
	for _, gen := range []int{0, 1, 7, 123456} {
		got, ok := parseInstanceEventsGeneration(instanceEventsFilename(gen))
		if !ok || got != gen {
			t.Fatalf("parseInstanceEventsGeneration(%q) = (%d, %v), want (%d, true)",
				instanceEventsFilename(gen), got, ok, gen)
		}
	}
}

func TestCompactInstanceEventsSweepsStrandedGenerationsAndReportsCleanup(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Generations 0-2 were stranded by earlier cleanup failures; generation 4
	// is current and holds the records this compaction acts on.
	for _, gen := range []int{0, 1, 2} {
		if err := os.WriteFile(filepath.Join(dir, instanceEventsFilename(gen)), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := eventLine(1, old, "") + "\n" + eventLine(2, recent, "") + "\n"
	if err := os.WriteFile(filepath.Join(dir, instanceEventsFilename(4)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := advanceInstanceEventsPointer(dir, 4); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	result, err := CompactInstanceEvents(dir, cutoff, cutoff, false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	if result.Dropped != 1 || result.Kept != 1 {
		t.Fatalf("compaction = %+v, want Dropped 1 Kept 1", result)
	}
	if result.StaleGenerationCleanupErr != nil {
		t.Fatalf("unexpected cleanup error: %v", result.StaleGenerationCleanupErr)
	}
	if result.StaleGenerationsRemoved != 3 {
		t.Fatalf("StaleGenerationsRemoved = %d, want 3", result.StaleGenerationsRemoved)
	}
	for _, gen := range []int{0, 1, 2} {
		if _, statErr := os.Stat(filepath.Join(dir, instanceEventsFilename(gen))); !os.IsNotExist(statErr) {
			t.Fatalf("stranded generation %d survived compaction, stat err = %v", gen, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, instanceEventsFilename(4))); statErr != nil {
		t.Fatalf("previous generation 4 must survive for in-flight readers: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, instanceEventsFilename(5))); statErr != nil {
		t.Fatalf("new generation 5 missing: %v", statErr)
	}
}

func TestCompactInstanceEventsSucceedsWhenStaleCleanupFails(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	body := eventLine(1, old, "") + "\n" + eventLine(2, recent, "") + "\n"
	if err := os.WriteFile(filepath.Join(dir, instanceEventsFilename(3)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := advanceInstanceEventsPointer(dir, 3); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory in a stale generation's place cannot be removed,
	// standing in for the sharing/permission failure this guards against.
	if err := os.MkdirAll(filepath.Join(dir, instanceEventsFilename(2), "held"), 0o755); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	result, err := CompactInstanceEvents(dir, cutoff, cutoff, false)
	if err != nil {
		t.Fatalf("a stale-generation cleanup failure must not fail compaction: %v", err)
	}
	if result.Dropped != 1 {
		t.Fatalf("compaction = %+v, want Dropped 1", result)
	}
	if result.StaleGenerationCleanupErr == nil {
		t.Fatal("cleanup failure was not surfaced on the compaction result")
	}
	if !strings.Contains(result.StaleGenerationCleanupErr.Error(), instanceEventsFilename(2)) {
		t.Fatalf("diagnostic %q does not name the stale generation", result.StaleGenerationCleanupErr)
	}
	gen, err := resolveInstanceEventsGeneration(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 4 {
		t.Fatalf("generation pointer = %d, want 4: compaction output must still be live", gen)
	}
}
