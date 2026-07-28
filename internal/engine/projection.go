package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/journal"
)

// ErrUnprojectable marks a history whose journal projection failed closed
// (#629): an op or event the projection does not recognize is an error
// surfaced on the run, never a silently skipped event.
var ErrUnprojectable = errors.New("engine: history is not projectable to a journal")

// projectableEventTypes is the closed set of event types the engine workflow
// emits. An op naming anything else fails the projection closed — the same
// stance the journal contract takes on producers inventing dialects.
var projectableEventTypes = map[journal.EventType]bool{
	journal.EventRunStarted:    true,
	journal.EventRunFinished:   true,
	journal.EventStageStarted:  true,
	journal.EventStageFinished: true,
	journal.EventGateStarted:   true,
	journal.EventGatePaused:    true,
	journal.EventGateEvaluated: true,
	journal.EventRefTouched:    true,
	journal.EventError:         true,
}

var projectableAttemptClasses = map[journal.AttemptClass]bool{
	"":                    true,
	journal.AttemptPolicy: true,
	journal.AttemptInfra:  true,
	journal.AttemptHuman:  true,
}

// projectionClock replays the workflow-deterministic op timestamps into the
// journal writer, which stamps event times and checkpoint times from its
// clock. Projecting the same history twice therefore yields byte-identical
// journals — the #629 determinism criterion — instead of wall-clock drift.
type projectionClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *projectionClock) set(t time.Time) {
	c.mu.Lock()
	c.current = t
	c.mu.Unlock()
}

func (c *projectionClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// ProjectRun writes one completed engine run's journal projection into the
// standard runs/<id>/ layout under runsDir, through the same journal writer
// the local runner uses — layout, digests, scrubbing, and durability come
// from internal/journal, so there is no engine-specific journal dialect.
// It fails closed (ErrUnprojectable) on anything it does not recognize.
// Returns the run directory.
func ProjectRun(runsDir string, proj JournalProjection) (string, error) {
	if err := validateProjection(proj); err != nil {
		return "", err
	}

	inputs := map[string][]byte{
		journal.PinnedWorkflowGraphInputName: []byte(proj.Graph),
	}
	if proj.Item != nil {
		b, err := json.Marshal(proj.Item)
		if err != nil {
			return "", fmt.Errorf("engine: marshal item snapshot: %w", err)
		}
		inputs["item"] = b
	}

	id := proj.Identity
	id.StartedAt = proj.Ops[0].Time

	clock := &projectionClock{}
	clock.set(proj.Ops[0].Time)
	jr, err := journal.Create(runsDir, id, inputs, journal.WithClock(clock.now))
	if err != nil {
		return "", fmt.Errorf("engine: create projected journal for run %q: %w", id.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	// journal.Create already appended the opening run.started (op 0);
	// validateProjection pinned its shape. Replay the rest.
	artifactRefs := map[string]journal.Ref{}
	for i, op := range proj.Ops[1:] {
		clock.set(op.Time)
		switch op.Kind {
		case opArtifact:
			a := op.Artifact
			var ref journal.Ref
			var recErr error
			if a.Stage != "" {
				ref, recErr = jr.RecordStageArtifact(a.Stage, a.Attempt, a.Class, a.Name, a.Data)
			} else {
				ref, recErr = jr.RecordArtifact(a.Name, a.Data)
			}
			if recErr != nil {
				return "", fmt.Errorf("engine: project artifact %q (op %d): %w", a.Name, i+1, recErr)
			}
			artifactRefs[a.Name] = ref
		case opAppend:
			ev := *op.Event
			if ev.Type == journal.EventGateEvaluated && ev.Name != "" {
				// The verdict artifact was recorded just above; the event's Ref
				// points at it, exactly as internal/gate's recordVerdict wires
				// the two together.
				ref, ok := artifactRefs[ev.Name]
				if !ok {
					return "", fmt.Errorf("%w: gate.evaluated (op %d) references unrecorded artifact %q", ErrUnprojectable, i+1, ev.Name)
				}
				ev.Ref = &ref
			}
			if err := jr.Append(ev); err != nil {
				return "", fmt.Errorf("engine: project event %s (op %d): %w", ev.Type, i+1, err)
			}
		}
	}
	return jr.Dir(), nil
}

// validateProjection is the fail-closed gate: every op must be a shape the
// engine is known to produce, opening with run.started and closing with
// exactly one terminal run.finished.
func validateProjection(proj JournalProjection) error {
	if proj.Identity.RunID == "" {
		return fmt.Errorf("%w: identity has no run id", ErrUnprojectable)
	}
	if len(proj.Graph) == 0 {
		return fmt.Errorf("%w: projection carries no pinned workflow graph", ErrUnprojectable)
	}
	if len(proj.Ops) == 0 {
		return fmt.Errorf("%w: history produced no journal ops", ErrUnprojectable)
	}
	first := proj.Ops[0]
	if first.Kind != opAppend || first.Event == nil ||
		first.Event.Type != journal.EventRunStarted || first.Event.Status != string(journal.PhaseRunning) {
		return fmt.Errorf("%w: first op is not the run.started event", ErrUnprojectable)
	}
	for i, op := range proj.Ops {
		if err := validateOp(op, i, len(proj.Ops)); err != nil {
			return err
		}
	}
	last := proj.Ops[len(proj.Ops)-1]
	if last.Kind != opAppend || last.Event == nil || last.Event.Type != journal.EventRunFinished {
		return fmt.Errorf("%w: history has no terminal run.finished event", ErrUnprojectable)
	}
	if len(proj.SchedulerOps) > 1 {
		return fmt.Errorf("%w: history produced %d scheduler trigger events", ErrUnprojectable, len(proj.SchedulerOps))
	}
	for i, op := range proj.SchedulerOps {
		if op.Kind != opAppend || op.Event == nil || op.Event.Type != journal.EventTriggerFired {
			return fmt.Errorf("%w: scheduler op %d is not trigger.fired", ErrUnprojectable, i)
		}
		ev := op.Event
		if op.Time.IsZero() || ev.Workflow == "" || ev.Gaggle == "" || ev.RunID == "" ||
			ev.RunID != proj.Identity.RunID || ev.Reason != "scheduled" {
			return fmt.Errorf("%w: scheduler trigger op %d has incomplete identity", ErrUnprojectable, i)
		}
	}
	return nil
}

func validateOp(op JournalOp, i, total int) error {
	switch op.Kind {
	case opAppend:
		ev := op.Event
		if ev == nil {
			return fmt.Errorf("%w: append op %d carries no event", ErrUnprojectable, i)
		}
		if !projectableEventTypes[ev.Type] {
			return fmt.Errorf("%w: op %d has unknown event type %q", ErrUnprojectable, i, ev.Type)
		}
		if !projectableAttemptClasses[ev.AttemptClass] {
			return fmt.Errorf("%w: op %d has unknown attempt class %q", ErrUnprojectable, i, ev.AttemptClass)
		}
		if ev.Type == journal.EventRunFinished {
			switch journal.RunPhase(ev.Status) {
			case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
			default:
				return fmt.Errorf("%w: op %d run.finished has unknown terminal status %q", ErrUnprojectable, i, ev.Status)
			}
			if i != total-1 {
				return fmt.Errorf("%w: op %d is run.finished but %d ops follow it", ErrUnprojectable, i, total-1-i)
			}
		}
	case opArtifact:
		a := op.Artifact
		if a == nil {
			return fmt.Errorf("%w: artifact op %d carries no payload", ErrUnprojectable, i)
		}
		if a.Name == "" {
			return fmt.Errorf("%w: artifact op %d has no name", ErrUnprojectable, i)
		}
		if !projectableAttemptClasses[a.Class] {
			return fmt.Errorf("%w: artifact op %d has unknown attempt class %q", ErrUnprojectable, i, a.Class)
		}
	default:
		return fmt.Errorf("%w: op %d has unknown kind %q", ErrUnprojectable, i, op.Kind)
	}
	return nil
}

// projectionQuerier is the slice of the Temporal client the projection needs.
// client.Client satisfies it; the conformance harness adapts the test
// environment instead.
type projectionQuerier interface {
	QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...interface{}) (converter.EncodedValue, error)
}

// ProjectCompletedRun queries a run's journal projection from Temporal
// (replaying its history — the projection is a function of history, #629) and
// writes it into the standard runs/<id>/ layout under runsDir.
func ProjectCompletedRun(ctx context.Context, q projectionQuerier, workflowID, runsDir string) (string, error) {
	proj, err := queryProjection(ctx, q, workflowID)
	if err != nil {
		return "", err
	}
	return ProjectRun(runsDir, proj)
}

// ProjectCompletedScheduledRun projects both sides of a schedule fire: the
// standard trigger.fired event in scheduler/events.jsonl and the run journal.
func ProjectCompletedScheduledRun(ctx context.Context, q projectionQuerier, workflowID, runsDir, schedulerDir string) (string, error) {
	proj, err := queryProjection(ctx, q, workflowID)
	if err != nil {
		return "", err
	}
	if err := ProjectSchedulerEvents(schedulerDir, proj); err != nil {
		return "", err
	}
	return ProjectRun(runsDir, proj)
}

func queryProjection(ctx context.Context, q projectionQuerier, workflowID string) (JournalProjection, error) {
	val, err := q.QueryWorkflow(ctx, workflowID, "", JournalQuery)
	if err != nil {
		return JournalProjection{}, fmt.Errorf("engine: query journal projection for %q: %w", workflowID, err)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		return JournalProjection{}, fmt.Errorf("engine: decode journal projection for %q: %w", workflowID, err)
	}
	return proj, nil
}

// ProjectSchedulerEvents appends a scheduled run's instance-level projection.
// A sequential retry is a no-op for an already-projected run ID.
func ProjectSchedulerEvents(schedulerDir string, proj JournalProjection) error {
	if err := validateProjection(proj); err != nil {
		return err
	}
	if len(proj.SchedulerOps) == 0 {
		return nil
	}
	clock := &projectionClock{}
	log, _, err := journal.OpenInstanceLog(schedulerDir, journal.WithClock(clock.now))
	if err != nil {
		return fmt.Errorf("engine: open projected scheduler journal: %w", err)
	}
	defer func() { _ = log.Close() }()
	existing, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		return fmt.Errorf("engine: read projected scheduler journal: %w", err)
	}
	projected := make(map[string]bool)
	for _, ev := range existing {
		if ev.Type == journal.EventTriggerFired {
			projected[ev.RunID] = true
		}
	}
	for i, op := range proj.SchedulerOps {
		if projected[op.Event.RunID] {
			continue
		}
		clock.set(op.Time)
		if err := log.Append(*op.Event); err != nil {
			return fmt.Errorf("engine: project scheduler event %d: %w", i, err)
		}
		projected[op.Event.RunID] = true
	}
	return nil
}
