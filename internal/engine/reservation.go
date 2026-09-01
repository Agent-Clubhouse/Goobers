package engine

import (
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// ReserveRun builds the live-journal batch that claims a run id for an
// engine-driven run BEFORE the workflow exists.
//
// # Why a reservation exists at all
//
// An engine run's journal is created by the workflow's FIRST emit, and the
// first emit cannot happen until a worker picks the task up and the first
// stage boundary is reached — up to stageScheduleToStart (15m) after
// TemporalStarter.Start returns, and unbounded if no worker is polling the
// queue. Between those two instants the run exists in Temporal and nowhere
// else: there is no runs/<id>/ directory, so the daemon's boot scan cannot
// see it, its scheduler slot is not reconstructible, and a daemon restart in
// that window re-admits the SAME workflow under a fresh run id while the
// first one keeps executing. The first run's terminal hooks then never fire —
// its claims are never released and its circuit breaker never records
// (decision 005 D1, "start-to-first-emit"; finding 002 hazard 6).
//
// Writing the header first closes the window: after Emit returns, runs/<id>/
// exists on disk with run.yaml naming driver: engine, so the ordinary boot
// scan sees the run and hands it to the engine reattach path like any other
// interrupted engine run.
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

// AbandonReservation builds the batch that CLOSES a reservation whose workflow
// was never started.
//
// ReserveRun deliberately writes runs/<id>/ before Temporal is called, so a
// daemon that dies in the start window leaves a record rather than nothing.
// The cost is that a start which FAILS — an unreachable frontend, a namespace
// error, a deadline — leaves behind a run directory stuck in phase running
// with no workflow that will ever finish it. Nothing reclaims such a
// directory: the orphan pruner skips anything holding a run.yaml, and the
// stalled-run sweep keeps trying to cancel a workflow that does not exist,
// journaling a fresh sweep failure on every tick, forever. During a Temporal
// outage that is one immortal zombie run per scheduler tick per engine lane.
//
// So the reservation is compensated: the same recorder that opened it records
// the run_failed cause and a terminal run.finished, and the run reads as a
// failed run — which is exactly what it is.
//
// cause is the start failure's text; it becomes the run_failed cause an
// operator reads.
func AbandonReservation(in RunInput, startedAt, finishedAt time.Time, cause string) (livejournal.EmitRequest, error) {
	req, err := ReserveRun(in, startedAt)
	if err != nil {
		return livejournal.EmitRequest{}, err
	}
	if finishedAt.IsZero() {
		finishedAt = startedAt
	}
	if cause == "" {
		cause = "engine dispatch failed to start the workflow"
	}
	// Rebuilt rather than appended to req.Ops, because emit keys are assigned
	// over the whole projection: appending to the already-keyed batch would
	// give the terminal ops keys that disagree with the ones the same
	// recorder would mint.
	in.Item = normalizeItemIntegrity(in.Item)
	m, err := wf.Compile(
		wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec},
		wf.WithPreviewFeatures(in.previewFeaturesEnabled()),
	)
	if err != nil {
		return livejournal.EmitRequest{}, fmt.Errorf("engine: abandon run %s: compile workflow: %w", in.RunID, err)
	}
	rec, err := newRunJournalRecorder(in, m)
	if err != nil {
		return livejournal.EmitRequest{}, fmt.Errorf("engine: abandon run %s: %w", in.RunID, err)
	}
	rec.appendAt(startedAt, journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)})
	rec.appendAt(finishedAt, journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "run_failed", Message: cause},
	})
	rec.appendAt(finishedAt, journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseFailed),
	})
	rec.assignEmitKeys()
	ops := make([]livejournal.Op, 0, len(rec.proj.Ops))
	for _, op := range rec.proj.Ops {
		ops = append(ops, liveOpFrom(op))
	}
	req.Ops = ops
	return req, nil
}
