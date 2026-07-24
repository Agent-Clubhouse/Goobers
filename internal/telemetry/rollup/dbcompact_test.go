package rollup

import (
	"path/filepath"
	"testing"
	"time"
)

// TestPruneSchedulerBeforeAndCompact covers the #1412 rollup side: aged
// scheduler rows are deleted by occurred_at and the store stays queryable after
// VACUUM.
func TestPruneSchedulerBeforeAndCompact(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, []string{
		instanceEventLine(1, "trigger.fired", `"workflow":"a","reason":"scheduled"`),
		instanceEventLine(2, "error", `"error":{"code":"boom","message":"old failure"}`),
		instanceEventLine(3, "trigger.fired", `"workflow":"a","reason":"scheduled"`),
		instanceEventLine(4, "trigger.fired", `"workflow":"a","reason":"scheduled"`),
	}); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}

	// instanceEventLine stamps event N at fixtureStart+N seconds; cut off before
	// seq 3 so seq 1 and 2 (which includes the error row) are pruned.
	cutoff := fixtureStart.Add(3 * time.Second)
	removed, err := db.PruneSchedulerBefore(cutoff)
	if err != nil {
		t.Fatalf("PruneSchedulerBefore: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed scheduler_events = %d, want 2", removed)
	}
	events, err := db.SchedulerEvents("")
	if err != nil {
		t.Fatalf("SchedulerEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("remaining scheduler events = %d, want 2: %#v", len(events), events)
	}
	sigs, err := db.TopErrorSignatures(StatsRequest{}, 10)
	if err != nil {
		t.Fatalf("TopErrorSignatures: %v", err)
	}
	if len(sigs) != 0 {
		t.Fatalf("error signatures after prune = %#v, want 0 (the error row was pruned)", sigs)
	}

	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	// Still fully usable after VACUUM.
	if events, err = db.SchedulerEvents(""); err != nil || len(events) != 2 {
		t.Fatalf("post-compact SchedulerEvents = %d (err %v), want 2", len(events), err)
	}
}

// TestPruneSchedulerBeforeZeroCutoffIsNoop guards the zero-time guard.
func TestPruneSchedulerBeforeZeroCutoffIsNoop(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, []string{
		instanceEventLine(1, "trigger.fired", `"workflow":"a","reason":"scheduled"`),
	}); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}
	removed, err := db.PruneSchedulerBefore(time.Time{})
	if err != nil {
		t.Fatalf("PruneSchedulerBefore(zero): %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d on zero cutoff, want 0", removed)
	}
}
