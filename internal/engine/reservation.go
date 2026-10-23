package engine

import (
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// ReserveRun builds the idempotent live-journal batch for an owned engine run.
// Provisional StartIntent custody closes the pre-Temporal crash window without
// claiming ownership. The caller publishes this batch only after the start
// fence classifies the execution as mine or adopt.
//
// # Why the header must be byte-identical to the workflow's
//
// livejournal.Writer.applyOp ABSORBS a run.started append against a journal
// that already has one (its dedupe path), and Emit's Open header is only
// honored when the journal is created. So whichever of the two writes lands
// first authors run.yaml permanently, and the workflow's own header is
// silently discarded. That makes the reservation header the run's real
// identity, which is why it is built from the same newRunJournalRecorder the
// workflow uses rather than assembled by hand here: a field added to the
// workflow's identity that this function did not know about would produce a
// permanently wrong run.yaml on precisely the runs the reservation protects.
//
// The one field that legitimately differs is the run.started event's TIME:
// the reservation stamps the daemon's wall clock (the true admission instant)
// and the workflow would stamp workflow.Now at its first stage boundary.
// StartedAt is descriptive, not normative — DiffLiveJournal compares the
// conformance view of the EVENT stream, not run.yaml timestamps — and the
// daemon's value is the more honest one.
//
// startedAt must be non-zero; it is the run's admission instant.
func ReserveRun(in RunInput, startedAt time.Time) (livejournal.EmitRequest, error) {
	if in.RunID == "" {
		return livejournal.EmitRequest{}, errors.New("engine: reserve run: run id is required")
	}
	if startedAt.IsZero() {
		return livejournal.EmitRequest{}, errors.New("engine: reserve run: started-at is required")
	}
	// Mirror run()'s two pre-journal normalizations exactly: the item
	// integrity stamp and the preview-gated compile. Both feed the identity
	// and input snapshots the header carries, so skipping either here would
	// reintroduce the very drift this function exists to prevent.
	in.Item = normalizeItemIntegrity(in.Item)
	m, err := wf.Compile(
		wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec},
		wf.WithPreviewFeatures(in.previewFeaturesEnabled()),
	)
	if err != nil {
		return livejournal.EmitRequest{}, fmt.Errorf("engine: reserve run %s: compile workflow: %w", in.RunID, err)
	}
	rec, err := newRunJournalRecorder(in, m)
	if err != nil {
		return livejournal.EmitRequest{}, fmt.Errorf("engine: reserve run %s: %w", in.RunID, err)
	}
	rec.appendAt(startedAt, journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)})
	rec.assignEmitKeys()
	ops := make([]livejournal.Op, 0, len(rec.proj.Ops))
	for _, op := range rec.proj.Ops {
		ops = append(ops, liveOpFrom(op))
	}
	return livejournal.EmitRequest{
		RunID:  rec.proj.Identity.RunID,
		Gaggle: rec.proj.Identity.Gaggle,
		Open: &livejournal.OpenHeader{
			Identity:               rec.proj.Identity,
			Item:                   rec.proj.Item,
			Graph:                  rec.proj.Graph,
			Definition:             rec.proj.Definition,
			GateGooberCapabilities: rec.proj.GateGooberCapabilities,
		},
		Ops: ops,
	}, nil
}

// TerminatedRunErrorCode is the run_failed cause code recorded when the daemon
// terminated an engine run's workflow because the engine never honoured the
// stall sweep's cancellation (#5407).
const TerminatedRunErrorCode = "engine_workflow_terminated"

// Emit keys of the daemon-authored closure. They are fixed per run, so a
// retried closure deduplicates instead of appending a second terminal.
const (
	terminatedRunCauseKey    = "daemon/engine-terminated/cause"
	terminatedRunFinishedKey = "daemon/engine-terminated/run.finished"
)

// TerminatedRunClosure builds the batch that CLOSES the journal of an
// engine run whose workflow the daemon terminated.
//
// Cancellation is the engine's own stop: its cancel arm journals run_failed and
// run.finished(aborted). That arm is workflow code, so it only runs when a
// worker picks the cancellation up. A worker that cannot poll (the versioned
// routing outage in #5407) never does, and a terminated workflow runs no code
// at all — nothing on the engine side will ever write this run's terminal. The
// daemon writes it instead, in the cancel arm's shape, only after Temporal has
// confirmed the workflow is closed.
//
// The batch carries no Open header: it is only ever applied to a journal that
// already exists (the reservation, or anything the workflow wrote after it).
func TerminatedRunClosure(runID, gaggle string, at time.Time, cause string) livejournal.EmitRequest {
	return livejournal.EmitRequest{
		RunID:  runID,
		Gaggle: gaggle,
		Ops: []livejournal.Op{
			{Kind: livejournal.OpAppend, Key: terminatedRunCauseKey, Time: at, Event: &journal.Event{
				Type:  journal.EventError,
				Error: &journal.ErrorDetail{Code: "run_failed", Message: cause},
			}},
			{Kind: livejournal.OpAppend, Key: terminatedRunFinishedKey, Time: at, Event: &journal.Event{
				Type: journal.EventRunFinished, Status: string(journal.PhaseAborted), Disposition: journal.RunDispositionProduced,
				TerminalCause: &journal.TerminalCause{
					Schema: journal.TerminalCauseSchema, Phase: journal.PhaseAborted,
					Classification: journal.TerminalInfrastructureFailure, SelectorKind: "condition",
					Code: TerminatedRunErrorCode, Message: cause, CausalEmitKey: terminatedRunCauseKey,
				},
			}},
		},
	}
}
