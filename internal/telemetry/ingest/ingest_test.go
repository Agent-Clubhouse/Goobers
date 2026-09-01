package ingest_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry/ingest"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestRunIntakeObserverRecordsEveryRunInBurst(t *testing.T) {
	store, err := intake.Open(filepath.Join(t.TempDir(), intake.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	observe := ingest.RunIntakeObserver(store, nil)

	for index, runID := range []string{"run-a", "run-b", "run-c", "run-d", "run-e"} {
		observe(runID, uint64(index+2))
	}

	pending, err := store.Pending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 5 {
		t.Fatalf("pending markers = %d, want 5", len(pending))
	}
	for index, marker := range pending {
		if marker.SourceSeq != uint64(index+2) {
			t.Fatalf("marker %s sequence = %d, want %d", marker.RunID, marker.SourceSeq, index+2)
		}
	}
}

func TestRunIntakeObserverNilStoreHasNoHook(t *testing.T) {
	if observe := ingest.RunIntakeObserver(nil, nil); observe != nil {
		t.Fatal("RunIntakeObserver with no watermark store = non-nil hook, want nil")
	}
}

// TestRunIntakeRecordsRunJournalWatermark covers the writer-side half of
// #1922/#1923: a finished run records a REAL journal sequence, not a
// placeholder, or the projector's `source_seq <= projectedSeq` guard would
// acknowledge unapplied work.
func TestRunIntakeRecordsRunJournalWatermark(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	const runID = "0af7651916cd43dd8448eb211c80319c"

	run, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "wf"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, RunID: runID, Stage: "implement"}); err != nil {
		t.Fatal(err)
	}
	wantSeq := run.Seq()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := intake.Open(filepath.Join(root, intake.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ingest.RunIntake(store, l, runID, nil)

	pending, err := store.Pending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].RunID != runID {
		t.Fatalf("pending markers = %+v, want one marker for %s", pending, runID)
	}
	if pending[0].SourceSeq != wantSeq {
		t.Fatalf("marker sequence = %d, want the run journal's highest sequence %d", pending[0].SourceSeq, wantSeq)
	}
}

// TestRunIntakeLogsUnknownRun asserts the "best-effort but never silent"
// contract (#246): a run directory that cannot be resolved is recorded, not
// swallowed.
func TestRunIntakeLogsUnknownRun(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)

	store, err := intake.Open(filepath.Join(root, intake.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	ingest.RunIntake(store, l, "run-does-not-exist", log)

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type == journal.EventError && ev.Error != nil && ev.Error.Code == "read_model_run_dir_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a read_model_run_dir_failed event, got: %+v", events)
	}
}

func TestRunIntakeNilStoreIsNoOp(t *testing.T) {
	ingest.RunIntake(nil, instance.NewLayout(t.TempDir()), "run-missing", nil)
}

// TestRunTelemetryLogsForcedFailure is issue #246's third fix: a
// swallowed rollup-ingest error used to leave nothing but a bare `_ =` — no
// visible trace anywhere that the rollup silently fell behind. This forces
// IngestRun to fail (a closed *rollup.DB) and asserts the failure is visible
// in the instance log, not merely absorbed.
func TestRunTelemetryLogsForcedFailure(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)

	db, err := rollup.Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Force IngestRun/IngestSchedulerLog to fail deterministically, without
	// relying on any particular on-disk run-directory shape.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	ingest.RunTelemetry(nil, db, nil, l, "run-forced-failure", log)

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type == journal.EventError && ev.RunID == "run-forced-failure" && ev.Error != nil &&
			strings.Contains(ev.Error.Code, "telemetry_ingest") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a telemetry_ingest_* error event for run-forced-failure, got: %+v", events)
	}
}

// TestRunTelemetryNilLogDoesNotPanic proves LogFailure's nil-log
// guard holds — RunTelemetry is called from contexts (tests, a
// standalone db) where no instance log may be wired.
func TestRunTelemetryNilLogDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	db, err := rollup.Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ingest.RunTelemetry(nil, db, nil, l, "run-nil-log", nil)
}

func TestRunTelemetryNilDBIsNoOp(t *testing.T) {
	ingest.RunTelemetry(nil, nil, nil, instance.NewLayout(t.TempDir()), "run-no-db", nil)
}

// TestSchedulerTelemetryLogsForcedFailure asserts the scheduler-side refresh
// keeps the same best-effort-but-never-silent contract as the run-side ingest.
func TestSchedulerTelemetryLogsForcedFailure(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)

	db, err := rollup.Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	ingest.SchedulerTelemetry(context.Background(), nil, db, l.SchedulerDir(), log)

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type == journal.EventError && ev.Error != nil &&
			ev.Error.Code == "telemetry_ingest_scheduler_log_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a telemetry_ingest_scheduler_log_failed event, got: %+v", events)
	}
}

func TestSchedulerTelemetryNilDBIsNoOp(t *testing.T) {
	ingest.SchedulerTelemetry(context.Background(), nil, nil, t.TempDir(), nil)
}
