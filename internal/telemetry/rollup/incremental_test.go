package rollup

import (
	"path/filepath"
	"testing"
)

// schedulerEventTypes returns the ingested scheduler events' types in seq order,
// the shape the incremental-ingest tests assert on.
func schedulerEventTypes(t *testing.T, db *DB) []string {
	t.Helper()
	events, err := db.SchedulerEvents("")
	if err != nil {
		t.Fatalf("SchedulerEvents: %v", err)
	}
	types := make([]string, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	return types
}

func schedulerCursorRow(t *testing.T, db *DB) (byteOffset int64, lastSeq uint64, present bool) {
	t.Helper()
	err := db.sql.QueryRow(`SELECT byte_offset, last_seq FROM scheduler_ingest_cursor WHERE id = 1`).
		Scan(&byteOffset, &lastSeq)
	if err != nil {
		return 0, 0, false
	}
	return byteOffset, lastSeq, true
}

// firstFive / firstEight share the deterministic prefix produced by
// instanceEventLine, so appending to the journal (rewriting with a superset)
// leaves the already-ingested bytes byte-identical — exactly how the real
// append-only journal grows.
func firstFive() []string {
	return []string{
		instanceEventLine(1, "trigger.fired", `"workflow":"nominate","reason":"scheduled"`),
		instanceEventLine(2, "tick.skipped", `"workflow":"nominate","reason":"cap"`),
		instanceEventLine(3, "run.started", `"workflow":"nominate","runId":"`+fixtureRunID+`"`),
		instanceEventLine(4, "run.finished", `"workflow":"nominate","runId":"`+fixtureRunID+`","status":"completed"`),
		instanceEventLine(5, "error", `"error":{"code":"claim_recovery_failed","message":"corrupt claims ledger"}`),
	}
}

func nextThree() []string {
	return []string{
		instanceEventLine(6, "trigger.fired", `"workflow":"implement","reason":"scheduled"`),
		instanceEventLine(7, "run.started", `"workflow":"implement","runId":"`+fixtureRunID+`"`),
		instanceEventLine(8, "error", `"error":{"code":"stalled_run_sweep_failed","message":"boom"}`),
	}
}

// TestIngestSchedulerLogAppendsIncrementally is the core #1411 acceptance test:
// re-ingesting after the journal grows adds only the new events, never
// duplicates, and advances the cursor past the appended tail.
func TestIngestSchedulerLogAppendsIncrementally(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, firstFive()); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("first IngestSchedulerLog: %v", err)
	}
	if got := schedulerEventTypes(t, db); len(got) != 5 {
		t.Fatalf("events after first ingest = %d, want 5: %v", len(got), got)
	}
	offset1, lastSeq1, present := schedulerCursorRow(t, db)
	if !present || lastSeq1 != 5 || offset1 <= 0 {
		t.Fatalf("cursor after first ingest = (offset %d, lastSeq %d, present %v), want offset>0 lastSeq 5", offset1, lastSeq1, present)
	}

	// Journal grows: rewrite with the same five lines plus three new ones. The
	// prefix bytes are identical, so incremental ingest resumes at offset1.
	if err := writeInstanceEvents(t, schedulerDir, append(firstFive(), nextThree()...)); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("second IngestSchedulerLog: %v", err)
	}
	got := schedulerEventTypes(t, db)
	if len(got) != 8 {
		t.Fatalf("events after incremental ingest = %d, want 8 (no dupes): %v", len(got), got)
	}
	offset2, lastSeq2, _ := schedulerCursorRow(t, db)
	if lastSeq2 != 8 || offset2 <= offset1 {
		t.Fatalf("cursor after incremental ingest = (offset %d, lastSeq %d), want offset>%d lastSeq 8", offset2, lastSeq2, offset1)
	}

	// The new error event must be captured too.
	sigs, err := db.TopErrorSignatures(StatsRequest{}, 10)
	if err != nil {
		t.Fatalf("TopErrorSignatures: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("error signatures = %#v, want 2 (claim_recovery_failed + stalled_run_sweep_failed)", sigs)
	}
}

// TestIngestSchedulerLogNoOpReingestIsIdempotent proves a steady-state ingest
// with nothing new added does not change the stored rows or the cursor — the
// property that stops the per-tick WAL churn (#1410).
func TestIngestSchedulerLogNoOpReingestIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, firstFive()); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	offset1, lastSeq1, _ := schedulerCursorRow(t, db)

	for i := 0; i < 3; i++ {
		if err := db.IngestSchedulerLog(schedulerDir); err != nil {
			t.Fatalf("re-ingest %d: %v", i, err)
		}
	}
	if got := schedulerEventTypes(t, db); len(got) != 5 {
		t.Fatalf("events after no-op re-ingests = %d, want 5: %v", len(got), got)
	}
	offset2, lastSeq2, _ := schedulerCursorRow(t, db)
	if offset2 != offset1 || lastSeq2 != lastSeq1 {
		t.Fatalf("cursor drifted on no-op re-ingest: (%d,%d) -> (%d,%d)", offset1, lastSeq1, offset2, lastSeq2)
	}
}

// TestIngestSchedulerLogResumesAfterJournalShrinks covers rotation/compaction:
// when the journal is now shorter than the last resume offset, ingest re-reads
// from the head, and the seq watermark keeps already-stored events (dropped
// from the file but still in the rollup) from being lost or duplicated.
func TestIngestSchedulerLogResumesAfterJournalShrinks(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, firstFive()); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// Compaction: the on-disk journal now holds only the two newest events
	// (seq 6,7) — far shorter than the five-line file, forcing a head re-read.
	if err := writeInstanceEvents(t, schedulerDir, nextThree()[:2]); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("post-shrink ingest: %v", err)
	}
	// The five originally-ingested events remain; the two new ones are added.
	if got := schedulerEventTypes(t, db); len(got) != 7 {
		t.Fatalf("events after shrink+ingest = %d, want 7 (5 retained + 2 new): %v", len(got), got)
	}
	_, lastSeq, _ := schedulerCursorRow(t, db)
	if lastSeq != 7 {
		t.Fatalf("lastSeq after shrink = %d, want 7", lastSeq)
	}
}

// TestIngestSchedulerLogUpgradesFromFullReplay simulates a DB previously
// populated by the old delete-then-insert path (rows present, no cursor row):
// the first incremental ingest seeds its watermark from the stored MAX(seq) and
// appends only genuinely new events without duplicating the existing ones.
func TestIngestSchedulerLogUpgradesFromFullReplay(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, firstFive()); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}
	// Drop the cursor to look like a DB rolled up by the pre-#1411 code.
	if _, err := db.sql.Exec(`DELETE FROM scheduler_ingest_cursor`); err != nil {
		t.Fatalf("delete cursor: %v", err)
	}

	if err := writeInstanceEvents(t, schedulerDir, append(firstFive(), nextThree()...)); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("post-upgrade ingest: %v", err)
	}
	if got := schedulerEventTypes(t, db); len(got) != 8 {
		t.Fatalf("events after upgrade ingest = %d, want 8 (no dupes): %v", len(got), got)
	}
	_, lastSeq, present := schedulerCursorRow(t, db)
	if !present || lastSeq != 8 {
		t.Fatalf("cursor after upgrade = (lastSeq %d, present %v), want lastSeq 8", lastSeq, present)
	}
}

// TestIngestSchedulerLogMatchesFullReplay pins incremental ingest to the same
// result a from-scratch full ingest produces — the correctness contract that
// lets the delete-then-insert replay be removed.
func TestIngestSchedulerLogMatchesFullReplay(t *testing.T) {
	lines := append(firstFive(), nextThree()...)

	// Incremental: ingest the first five, then the full eight.
	incTmp := t.TempDir()
	incDir := filepath.Join(incTmp, "scheduler")
	if err := writeInstanceEvents(t, incDir, firstFive()); err != nil {
		t.Fatal(err)
	}
	incDB := openTestDB(t, incTmp)
	if err := incDB.IngestSchedulerLog(incDir); err != nil {
		t.Fatalf("incremental seed: %v", err)
	}
	if err := writeInstanceEvents(t, incDir, lines); err != nil {
		t.Fatal(err)
	}
	if err := incDB.IngestSchedulerLog(incDir); err != nil {
		t.Fatalf("incremental tail: %v", err)
	}

	// Full: a single ingest of all eight into a fresh DB.
	fullTmp := t.TempDir()
	fullDir := filepath.Join(fullTmp, "scheduler")
	if err := writeInstanceEvents(t, fullDir, lines); err != nil {
		t.Fatal(err)
	}
	fullDB := openTestDB(t, fullTmp)
	if err := fullDB.IngestSchedulerLog(fullDir); err != nil {
		t.Fatalf("full ingest: %v", err)
	}

	incTypes := schedulerEventTypes(t, incDB)
	fullTypes := schedulerEventTypes(t, fullDB)
	if len(incTypes) != len(fullTypes) {
		t.Fatalf("incremental events = %v, full events = %v", incTypes, fullTypes)
	}
	for i := range incTypes {
		if incTypes[i] != fullTypes[i] {
			t.Fatalf("event %d: incremental %q != full %q", i, incTypes[i], fullTypes[i])
		}
	}
}
