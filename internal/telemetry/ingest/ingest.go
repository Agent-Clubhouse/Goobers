// Package ingest holds the run-completion telemetry ingestion that feeds the
// local rollup and the read model's intake watermarks.
//
// It lives here rather than in cmd/goobers so the runner wiring stays
// construction-only: the CLI decides WHEN a run finished, this package decides
// what ingesting that run means. It cannot live in internal/telemetry itself —
// internal/telemetry/rollup imports internal/telemetry, so the ingestion side
// has to sit below both.
package ingest

import (
	"context"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// RunTelemetry incrementally ingests one finished run, plus a refresh
// of the scheduler decision log and spans, into the local telemetry rollup (issues
// #127/#128) — internal/telemetry/rollup/ingest.go's own doc comment already
// claimed IngestRun is meant to hook a run's completion ("call it once a run
// finishes"), but nothing in cmd/goobers ever called it; every `goobers
// telemetry`/`trace` query instead paid for a full rollup.Rebuild (an
// os.Remove + full rescan) just to stay correct, and scheduler/events.jsonl
// (trigger.fired/tick.skipped/claim.*) was never ingested at all. Called from
// both up.go (every scheduler-dispatched and resumed run) and run.go (the
// one-shot manual run — its scheduler log ingest is a no-op there, since
// `goobers run` never dispatches through the scheduler), regardless of the
// run's own error, so a failed run's errors/stage_attempts still show up in
// `goobers telemetry errors`. Best-effort: the rollup is derived state, never
// the source of truth, so an ingest failure here must never fail the run
// itself.
//
// FlushLocal MUST run before IngestRun reads spans.jsonl: the local journal
// exporter batches completed spans, but flushing the whole provider would also
// wait for a configured remote collector and delay scheduler-slot release.
func RunTelemetry(tel *telemetry.Client, db *rollup.DB, watermarks *intake.Store, l instance.Layout, runID string, log *journal.InstanceLog) {
	if tel != nil {
		if err := tel.FlushLocal(context.Background()); err != nil {
			LogFailure(log, runID, "telemetry_flush_failed", err)
		}
	}
	if db == nil {
		return
	}
	// Best-effort (the rollup is derived state, never the source of truth,
	// so a failure here must never fail the run) does NOT mean silent
	// (issue #246): a swallowed error here — e.g. the harness_transcripts PK
	// conflict on re-ingesting a resumed run — left the rollup silently
	// stale with nothing but a blank `_ =` to show for it. LogFailure
	// records it to the instance log, matching resumeInterruptedRuns' own
	// resume_unresolvable_workflow convention, without changing the
	// swallow-and-continue control flow.
	if err := db.IngestRun(context.Background(), filepath.Join(l.RunsDir(), runID)); err != nil {
		LogFailure(log, runID, "telemetry_ingest_run_failed", err)
	}
	SchedulerLog(db, l.SchedulerDir(), log, runID)
	RunIntake(watermarks, l, runID, log)
}

// RunIntake records a source watermark rather than projecting inline
// (#1922/#1923, §6.1).
//
// # The inversion
//
// This used to call ProjectRunDir here, from the writer, in-process, at run
// completion — the same coupling `IngestRun` has above. That dependency does not
// survive separating execution from serving, and it has a defect that shows up
// long before any separation: a run written while the daemon is down is never
// projected at all, and nothing notices.
//
// Now the writer records "this run advanced to sequence N" and forgets about it.
// The projector discovers the marker on its own schedule and applies it. A
// marker written while the daemon is down is still there when it starts, so the
// restart pass picks it up — which is the whole point.
//
// # Why it reads the journal for the sequence
//
// The watermark must carry a REAL sequence, not a placeholder. The
// acknowledgement guard is `source_seq <= projectedSeq`: if every writer
// recorded zero, an append racing a projection would be acknowledged away as
// though it had been applied. One journal read here replaces the much larger
// whole-run ingest that used to happen at this point.
//
// # Failure is degradation, not an error
//
// Best-effort but never silent (#246). A watermark that fails to record means
// that run is discovered by the repair sweep instead — slower, but complete. The
// alternative, failing a run because a read-model hint could not be written,
// would make the read model an availability dependency of execution.
func RunIntake(watermarks *intake.Store, l instance.Layout, runID string, log *journal.InstanceLog) {
	if watermarks == nil {
		return
	}
	dir, err := l.FindRunDir(runID)
	if err != nil {
		LogFailure(log, runID, "read_model_run_dir_failed", err)
		return
	}
	seq, err := lastJournalSeq(dir)
	if err != nil {
		LogFailure(log, runID, "read_model_intake_seq_failed", err)
		return
	}
	if err := watermarks.Observed(context.Background(), runID, seq); err != nil {
		LogFailure(log, runID, "read_model_intake_failed", err)
	}
}

// RunIntakeObserver returns the journal-advance hook that records an intake
// watermark as a run progresses, or nil when no watermark store is wired.
func RunIntakeObserver(watermarks *intake.Store, log *journal.InstanceLog) func(string, uint64) {
	if watermarks == nil {
		return nil
	}
	return func(runID string, seq uint64) {
		if err := watermarks.Observed(context.Background(), runID, seq); err != nil {
			LogFailure(log, runID, "read_model_intake_failed", err)
		}
	}
}

// lastJournalSeq reports the highest sequence in a run's journal.
//
// Takes the maximum rather than the last record's sequence: the live instance's
// journals contain duplicate and regressed sequences (1,394 duplicates and 119
// regressions, from #530), so "the last line" and "the highest sequence" are not
// the same number. The watermark has to be the highest, or the acknowledgement
// guard would let a projection at the true maximum acknowledge a marker that
// still represented unapplied work.
func lastJournalSeq(dir string) (uint64, error) {
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return 0, err
	}
	events, err := reader.Events()
	if err != nil {
		return 0, err
	}
	var highest uint64
	for _, event := range events {
		if event.Seq > highest {
			highest = event.Seq
		}
	}
	return highest, nil
}

// SchedulerTelemetry flushes pending spans and refreshes the scheduler decision
// log in the rollup, outside any single run's completion.
func SchedulerTelemetry(ctx context.Context, tel *telemetry.Client, db *rollup.DB, schedulerDir string, log *journal.InstanceLog) {
	if tel != nil {
		if err := tel.Flush(ctx); err != nil {
			LogFailure(log, "", "telemetry_flush_failed", err)
		}
	}
	if db == nil {
		return
	}
	SchedulerLog(db, schedulerDir, log, "")
}

// SchedulerLog ingests the scheduler decision log, recording — never
// returning — an ingest failure.
func SchedulerLog(db *rollup.DB, schedulerDir string, log *journal.InstanceLog, runID string) {
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		LogFailure(log, runID, "telemetry_ingest_scheduler_log_failed", err)
	}
}

// LogFailure appends a best-effort diagnostic event for a failed
// rollup ingest (issue #246) — nil-safe (log may be nil in a test/standalone
// context) and itself swallows its own Append error, since a logging
// failure must not cascade into a second failure mode.
func LogFailure(log *journal.InstanceLog, runID, code string, cause error) {
	if log == nil {
		return
	}
	_ = log.Append(journal.Event{
		Type: journal.EventError, RunID: runID,
		Error: &journal.ErrorDetail{Code: code, Message: cause.Error()},
	})
}
