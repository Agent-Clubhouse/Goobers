package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.temporal.io/sdk/converter"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
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
	journal.EventSpanRecorded:  true,
	// Placement provenance (#3515) is non-normative for CONFORMANCE but is
	// projectable: projectable and conformance-normative are different
	// questions, and span.recorded is the standing precedent — also excluded
	// from conformance, also projected. Without this entry a repair/backfill
	// re-projection of any history carrying a placement op fails closed on a
	// type the engine itself will emit (#3529).
	journal.EventRunnerPlacement: true,
}

// spanUnavailableErrorCode marks the EventError a projection appends in place
// of a span.recorded event it could not adopt (#2907) — see writeProjectedRun's
// opSpan case for why this is a soft failure rather than ErrUnprojectable.
const spanUnavailableErrorCode = "span_unavailable"

// SpanSource fetches a previously-recorded span's bytes by content digest, so
// the projection writer can adopt an executor-produced span (a harness
// transcript, JournalSpanOp) it cannot itself recompute — the workflow only
// ever carries the pointer (#2907). Satisfied by internal/blobstore.Store.
type SpanSource interface {
	Get(ctx context.Context, digest string) ([]byte, error)
}

// ProjectOption configures optional ProjectRun behavior.
type ProjectOption func(*projectConfig)

type projectConfig struct {
	spanSource SpanSource
}

// WithSpanSource configures ProjectRun to adopt recorded spans (harness
// transcripts) from src by digest. Without it — the default — a span op is
// recorded as a spanUnavailableErrorCode error event instead of dropped
// silently: see writeProjectedRun.
func WithSpanSource(src SpanSource) ProjectOption {
	return func(c *projectConfig) { c.spanSource = src }
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
//
// opts is additive: WithSpanSource lets a caller that has a fleet-wide blob
// store adopt recorded spans (harness transcripts) by digest. Without it, a
// span op is recorded as a spanUnavailableErrorCode error event rather than
// silently dropped or failing the whole run's projection — see
// writeProjectedRun's opSpan case for why availability must not be
// fail-closed like an unrecognized op.
func ProjectRun(runsDir string, proj JournalProjection, opts ...ProjectOption) (string, error) {
	if err := validateProjection(proj); err != nil {
		return "", err
	}
	cfg := &projectConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	finalDir := filepath.Join(runsDir, proj.Identity.RunID)
	replacePartial := false
	if journal.Recorded(finalDir) {
		complete, err := projectedJournalComplete(finalDir)
		if err != nil {
			return "", fmt.Errorf("engine: inspect existing projected journal for run %q: %w", proj.Identity.RunID, err)
		}
		if complete {
			return finalDir, nil
		}
		replacePartial = true
	}
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return "", fmt.Errorf("engine: create projected runs directory: %w", err)
	}
	stagingRoot := journal.RunCreationStagingDir(runsDir)
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return "", fmt.Errorf("engine: create projection staging root: %w", err)
	}
	stagingDir, err := os.MkdirTemp(stagingRoot, proj.Identity.RunID+"-projection-")
	if err != nil {
		return "", fmt.Errorf("engine: create projection staging directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(stagingDir)
		_ = os.RemoveAll(journal.RunCreationStagingDir(stagingDir))
	}()

	projectedDir, err := writeProjectedRun(stagingDir, proj, cfg)
	if err != nil {
		return "", err
	}
	if replacePartial {
		if err := os.RemoveAll(finalDir); err != nil {
			return "", fmt.Errorf("engine: remove partial projection for run %q: %w", proj.Identity.RunID, err)
		}
	}
	if err := os.Rename(projectedDir, finalDir); err != nil {
		if journal.Recorded(finalDir) {
			complete, inspectErr := projectedJournalComplete(finalDir)
			if inspectErr == nil && complete {
				return finalDir, nil
			}
		}
		return "", fmt.Errorf("engine: publish projected journal for run %q: %w", proj.Identity.RunID, err)
	}
	if err := durability.SyncDir(runsDir); err != nil {
		return "", fmt.Errorf("engine: sync projected runs directory: %w", err)
	}
	return finalDir, nil
}

func writeProjectedRun(runsDir string, proj JournalProjection, cfg *projectConfig) (string, error) {
	inputs := map[string][]byte{
		journal.PinnedWorkflowGraphInputName:      []byte(proj.Graph),
		journal.PinnedWorkflowDefinitionInputName: []byte(proj.Definition),
	}
	inputIntegrity := map[string]apiv1.Integrity{
		journal.PinnedWorkflowGraphInputName:      apiv1.IntegrityTrusted,
		journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
	}
	// The reviewer-goober capability map pinned into the run input at start
	// (#294): projected as a trusted input so the daemon credential plane
	// resolves an agentic gate's reviewer grants from the run's pin, never
	// the currently-served config (PR #3528).
	if len(proj.GateGooberCapabilities) > 0 {
		inputs[journal.PinnedGateGooberCapabilitiesInputName] = []byte(proj.GateGooberCapabilities)
		inputIntegrity[journal.PinnedGateGooberCapabilitiesInputName] = apiv1.IntegrityTrusted
	}
	if proj.Item != nil {
		item := normalizeItemIntegrity(proj.Item)
		b, err := json.Marshal(item)
		if err != nil {
			return "", fmt.Errorf("engine: marshal item snapshot: %w", err)
		}
		inputs["item"] = b
		inputIntegrity["item"] = item.Integrity
	}

	id := proj.Identity
	id.StartedAt = proj.Ops[0].Time

	clock := &projectionClock{}
	clock.set(proj.Ops[0].Time)
	jr, err := journal.Create(
		runsDir, id, inputs, journal.WithClock(clock.now), journal.WithInputIntegrity(inputIntegrity),
	)
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
			integrity := a.Integrity
			if integrity == "" {
				integrity = apiv1.IntegrityDerived
			}
			var ref journal.Ref
			var recErr error
			if a.Stage != "" {
				ref, recErr = jr.RecordStageArtifactWithIntegrity(a.Stage, a.Attempt, a.Class, a.Name, a.Data, integrity)
			} else {
				ref, recErr = jr.RecordArtifactWithIntegrity(a.Name, a.Data, integrity)
			}
			if recErr != nil {
				return "", fmt.Errorf("engine: project artifact %q (op %d): %w", a.Name, i+1, recErr)
			}
			artifactRefs[a.Name] = ref
		case opSpan:
			s := op.Span
			if s == nil {
				return "", fmt.Errorf("%w: span op %d carries no payload", ErrUnprojectable, i+1)
			}
			if err := adoptSpan(jr, cfg.spanSource, *s); err != nil {
				return "", fmt.Errorf("engine: project span %q (op %d): %w", s.Name, i+1, err)
			}
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
	if err := jr.Close(); err != nil {
		return "", fmt.Errorf("engine: close projected journal for run %q: %w", id.RunID, err)
	}
	return jr.Dir(), nil
}

// adoptSpan writes op into jr: fetched-and-recorded as span.recorded when src
// can supply the bytes, or a spanUnavailableErrorCode error event otherwise.
//
// A span's byte availability is a property of THIS projection attempt (blob
// store reachability), not of the workflow history it replays — re-running
// the same history against the same src should adopt the same span, but a
// caller with no src configured, or a store that is briefly unreachable,
// must not be indistinguishable from ErrUnprojectable's "an op or event this
// projection does not recognize." That is the case validateOp's structural
// checks reject unconditionally; this is a runtime availability gap in an op
// this projection DOES recognize, for a value already documented as
// diagnostic evidence rather than a stage output (apiv1.ResultEnvelope.
// Transcript). Downgrading it to a visible EventError — rather than either
// failing the whole run's projection or dropping it silently — keeps the run
// itself projectable and visible in every product surface (the #2895
// problem this must not reintroduce) while still surfacing the gap (#2907).
func adoptSpan(jr *journal.Run, src SpanSource, op JournalSpanOp) error {
	data, err := fetchSpan(src, op.Ref.Digest)
	if err != nil {
		return jr.Append(journal.Event{
			Type: journal.EventError, Stage: op.Stage, Attempt: op.Attempt, AttemptClass: op.Class,
			Error: &journal.ErrorDetail{
				Code:    spanUnavailableErrorCode,
				Message: fmt.Sprintf("span %q (%s): %v", op.Name, op.Ref.Digest, err),
			},
		})
	}
	_, err = jr.RecordSpanWithSchema(op.Stage, op.Name, op.DataSchema, data)
	return err
}

// fetchSpan fetches digest from src and confirms the bytes returned actually
// hash to it — src is an external dependency (a fleet blob store), and a
// wrong-content bug there must surface as an unavailable span, never a
// silently mismatched one.
func fetchSpan(src SpanSource, digest string) ([]byte, error) {
	if src == nil {
		return nil, errors.New("no span source configured")
	}
	if digest == "" {
		return nil, errors.New("span op has no digest")
	}
	data, err := src.Get(context.Background(), digest)
	if err != nil {
		return nil, err
	}
	if got := journal.Digest(data); got != digest {
		return nil, fmt.Errorf("fetched bytes hash to %s, want %s", got, digest)
	}
	return data, nil
}

func projectedJournalComplete(dir string) (bool, error) {
	rd, err := journal.OpenRead(dir)
	if err != nil {
		return false, err
	}
	events, err := rd.Events()
	if err != nil {
		return false, err
	}
	if len(events) == 0 {
		return false, nil
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished {
		return false, nil
	}
	switch journal.RunPhase(last.Status) {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true, nil
	default:
		return false, nil
	}
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
	if len(proj.Definition) == 0 {
		return fmt.Errorf("%w: projection carries no pinned workflow definition", ErrUnprojectable)
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
		if a.Integrity != "" && !a.Integrity.Valid() {
			return fmt.Errorf("%w: artifact op %d has unknown integrity %q", ErrUnprojectable, i, a.Integrity)
		}
		if !projectableAttemptClasses[a.Class] {
			return fmt.Errorf("%w: artifact op %d has unknown attempt class %q", ErrUnprojectable, i, a.Class)
		}
	case opSpan:
		s := op.Span
		if s == nil {
			return fmt.Errorf("%w: span op %d carries no payload", ErrUnprojectable, i)
		}
		if s.Name == "" {
			return fmt.Errorf("%w: span op %d has no name", ErrUnprojectable, i)
		}
		if s.Ref.Digest == "" {
			return fmt.Errorf("%w: span op %d has no digest", ErrUnprojectable, i)
		}
		if !projectableAttemptClasses[s.Class] {
			return fmt.Errorf("%w: span op %d has unknown attempt class %q", ErrUnprojectable, i, s.Class)
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
// claimID is the Schedule action workflow ID; its deterministic child owns the
// journal query.
func ProjectCompletedScheduledRun(ctx context.Context, q projectionQuerier, claimID, runsDir, schedulerDir string) (string, error) {
	proj, err := queryProjection(ctx, q, scheduledRunWorkflowID(claimID))
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
