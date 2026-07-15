package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// TestResetRateLimitWritesMarkerAndPreservesRuns is #315's acceptance at the
// CLI level: `goobers reset-rate-limit` writes the rate-reset marker under
// scheduler/ and leaves runs/ (the durable run journals) completely untouched
// — the whole point, versus the `rm -rf <instance>` habit it replaces.
func TestResetRateLimitWritesMarkerAndPreservesRuns(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)

	// Seed a run journal + a scheduler marker file, standing in for real
	// forensic history the reset must not destroy.
	runDir := filepath.Join(l.RunsDir(), "run-keepme")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(runDir, "events.jsonl")
	if err := os.WriteFile(sentinel, []byte(`{"type":"run.started"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "reset-rate-limit", root)
	if code != 0 {
		t.Fatalf("reset-rate-limit: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "runs/ preserved") {
		t.Fatalf("stdout = %q, want a runs/-preserved confirmation", stdout)
	}

	// Marker written under scheduler/.
	if _, err := os.Stat(filepath.Join(l.SchedulerDir(), "rate-reset")); err != nil {
		t.Fatalf("rate-reset marker not written: %v", err)
	}
	// runs/ untouched — the durable execution record survives.
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != `{"type":"run.started"}` {
		t.Fatalf("run journal was disturbed by reset: content=%q err=%v", got, err)
	}
}

// TestResetRateLimitRejectsNonInstance proves the command fails closed (usage
// exit 2) on a path that isn't an instance root, rather than scaffolding a
// stray scheduler/ marker somewhere unintended.
func TestResetRateLimitRejectsNonInstance(t *testing.T) {
	code, _, stderr := runArgs(t, "reset-rate-limit", t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2 for a non-instance path, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q, want a clear not-an-instance message", stderr)
	}
}
