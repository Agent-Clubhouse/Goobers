package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
	telemetryingest "github.com/goobers/goobers/internal/telemetry/ingest"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
)

// engineRuntime is the late-bound half of an engineStarter's wiring.
//
// It exists because of an ordering fact in up.go that cannot be reordered
// away: buildSchedulerDefinitions — which constructs every scheduler entry
// and therefore every Starter — runs BEFORE newLiveJournalWriter and
// newDaemonEngineClient. The scheduler entries need Starters at construction;
// the Starters need a Temporal client and a live journal writer that do not
// exist yet. Moving the engine dial earlier is not available: the dial's
// configuration comes from the same config load the definitions come from,
// and a config reload rebuilds the definitions without redialing.
//
// So each engineStarter holds a pointer to this shared holder, and up.go
// attaches the runtime once, after the client and writer exist. Attach is
// idempotent and safe against concurrent Start calls.
//
// An unattached runtime FAILS the dispatch. It does not silently fall back to
// the local runner: a lane that the selection predicate placed on the engine
// has stages pinned to remote runners, and running it in-process would
// execute them on the daemon's host — the exact placement violation the pins
// exist to prevent. Failing is visible in the scheduler's run.finished echo;
// a silent fallback would not be.
type engineRuntime struct {
	mu sync.RWMutex

	starter engine.Starter
	guards  *engineRunGuards
	live    *livejournal.Writer
	now     func() time.Time
}

// Attach wires the runtime. Called once from up.go after the Temporal client
// and the live journal writer exist.
func (r *engineRuntime) Attach(starter engine.Starter, guards *engineRunGuards, live *livejournal.Writer, now func() time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starter = starter
	r.guards = guards
	r.live = live
	r.now = now
}

// adoptFrom copies prev's attachment onto r.
//
// buildSchedulerDefinitions mints a FRESH holder on every call, and a config
// reload calls it again — so without this, every reloaded engine lane's
// Starter would point at a never-attached runtime and fail closed on every
// subsequent dispatch until the daemon restarted. Silently, because failing
// closed is the correct answer to a genuinely unattached runtime.
//
// Re-attaching rather than re-dialing is deliberate: the engine dial is
// boot-scoped (see the type comment), a reload has no client or writer to
// build from, and the ones the boot path built are still the right ones.
func (r *engineRuntime) adoptFrom(prev *engineRuntime) {
	if r == nil || prev == nil || r == prev {
		return
	}
	prev.mu.RLock()
	starter, guards, live, now := prev.starter, prev.guards, prev.live, prev.now
	prev.mu.RUnlock()
	if starter == nil && guards == nil && live == nil && now == nil {
		return
	}
	r.Attach(starter, guards, live, now)
}

// resolve returns the attached runtime, or an error naming what is missing.
func (r *engineRuntime) resolve() (engine.Starter, *engineRunGuards, *livejournal.Writer, func() time.Time, error) {
	if r == nil {
		return nil, nil, nil, nil, errEngineRuntimeUnattached
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.starter == nil || r.guards == nil {
		return nil, nil, nil, nil, errEngineRuntimeUnattached
	}
	now := r.now
	if now == nil {
		now = time.Now
	}
	return r.starter, r.guards, r.live, now, nil
}

var errEngineRuntimeUnattached = errors.New("engine runtime is not attached; this daemon has no Temporal client for engine dispatch")

// engineStarter dispatches one scheduler entry's runs onto the tier-3 engine.
// It is the localscheduler.Starter the per-entry selection installs in place
// of trackedStarter for a lane whose stages are all remotely pinned.
//
// # The blocking contract
//
// Start BLOCKS until the workflow closes, because that is what
// localscheduler.Starter means: the scheduler holds the workflow's
// concurrency slot for exactly as long as Start runs, and the phase Start
// returns is what it journals as the run's outcome. Returning early would
// release the slot under a live run and journal a fabricated terminal.
//
// That is why the trigger-plane dispatch-context fix
// (Scheduler.TriggerWithDispatchContext) is not an optional companion change
// but a precondition: the wait runs on the context dispatch was given, and
// the trigger plane used to give it the HTTP request's context.
type engineStarter struct {
	runtime *engineRuntime
	hooks   *engineTerminalHooks

	gaggle     string
	def        workflow.Definition
	spec       engineRunRequest
	layout     instance.Layout
	log        *journal.InstanceLog
	telemetry  *telemetry.Client
	rollupDB   *rollup.DB
	watermarks *intake.Store

	// allowPreviewFeatures is the instance's preview posture, pinned into
	// every RunInput this starter builds.
	allowPreviewFeatures bool
	// liveJournal mirrors the instance's journal-plane posture: an engine
	// run started by the daemon authors its journal live, which is what makes
	// the run visible mid-flight AND what makes the pre-Temporal reservation
	// possible at all.
	liveJournal bool

	wg *sync.WaitGroup
}

// Start dispatches one run onto the engine and waits for it.
func (s *engineStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	if s.wg != nil {
		s.wg.Add(1)
		defer s.wg.Done()
	}
	starter, guards, live, now, err := s.runtime.resolve()
	if err != nil {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, err)
	}
	if err := s.hooks.validate(req.Gaggle); err != nil {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, err)
	}

	spec := s.spec
	spec.runID = req.RunID
	spec.gaggle = req.Gaggle
	spec.project = req.RepoRef
	spec.item = req.Item
	spec.triggerKind = string(req.Trigger.Kind)
	spec.triggerRef = req.Trigger.Ref
	spec.gooberDigest = req.GooberDigest
	spec.liveJournal = s.liveJournal

	startSpec, err := engineRunSpec(spec)
	if err != nil {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, err)
	}
	in, err := engine.RunInputFor(s.def.Name, s.def, s.allowPreviewFeatures, startSpec)
	if err != nil {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, err)
	}

	// Reservation BEFORE the workflow exists. See engine.ReserveRun: between
	// TemporalStarter.Start returning and the workflow's first emit there is
	// a window (up to stageScheduleToStart) in which the run exists only in
	// Temporal, and a daemon restart inside it admits a second run of the
	// same workflow while the first keeps executing with its terminal hooks
	// never firing. Writing runs/<id>/run.yaml with driver: engine first
	// makes the boot scan able to see the run.
	//
	// A reservation failure REFUSES the dispatch rather than proceeding
	// unreserved: proceeding would re-open exactly the window this closes,
	// and no workflow has been started yet, so refusing costs one tick.
	var (
		reserved   bool
		reservedAt time.Time
	)
	if s.liveJournal && live != nil {
		reservedAt = now()
		reservation, rerr := engine.ReserveRun(in, reservedAt)
		if rerr != nil {
			return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, rerr)
		}
		if _, rerr := live.Emit(ctx, reservation); rerr != nil {
			return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: reserve run journal: %w", req.Gaggle, s.def.Name, rerr)
		}
		reserved = true
	}
	s.echo(req, journal.Event{
		Type:   journal.EventRunStarted,
		Status: string(journal.PhaseRunning),
		Reason: "engine dispatch admitted",
	})

	if _, err := starter.Start(ctx, in); err != nil {
		s.echo(req, journal.Event{
			Type:   journal.EventError,
			Reason: "engine dispatch failed to start",
			Error:  &journal.ErrorDetail{Code: "engine_start_failed", Message: err.Error()},
		})
		// Close the reservation out. It was written before Temporal was
		// called precisely so a crash in the window leaves a record; a start
		// that failed outright leaves that record with no workflow that will
		// ever finish it, and nothing reclaims a run directory holding a
		// run.yaml — the orphan pruner skips it and the stalled-run sweep
		// retries cancelling a workflow that does not exist on every tick,
		// forever. Best-effort, and on the daemon's context: a dispatch
		// cancelled by SIGTERM is exactly when this matters most.
		if reserved {
			s.abandonReservation(context.WithoutCancel(ctx), live, in, req, reservedAt, now(), err)
		}
		return localscheduler.StartResult{Phase: journal.PhaseFailed, FailureCode: "engine_start_failed", FailureMessage: err.Error()},
			fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, err)
	}

	var result engine.RunResult
	attachment := guards.awaitInto(ctx, req.RunID, &result)
	if !attachment.Settled {
		// The daemon never established the run's outcome. Report the error
		// WITHOUT firing terminal hooks: every hook here assumes the run is
		// over, and releasing this run's claims while its workflow is still
		// executing is the duplicate-driver hazard from the other end. The
		// slot is released by the scheduler when Start returns, and the boot
		// reattach scan picks the run up if this daemon restarts.
		return localscheduler.StartResult{Phase: journal.PhaseRunning}, fmt.Errorf(
			"engine dispatch for %s/%s: outcome unknown: %w", req.Gaggle, s.def.Name, attachment.Err)
	}
	phase := engineTerminalPhaseFor(result, attachment.Err)
	out := engineTerminalOutcome{
		RunID:    req.RunID,
		Gaggle:   req.Gaggle,
		Workflow: s.def.Name,
		Phase:    phase,
		Result:   result,
		Item:     req.Item,
		Err:      attachment.Err,
	}
	// The hooks run on the DAEMON's lifecycle context, not the dispatch one.
	// A dispatch context that is already cancelled (SIGTERM during the wait)
	// must not skip claim release: the run is over either way, and its claims
	// would otherwise be held until the lease expires.
	s.hooks.run(context.WithoutCancel(ctx), out)
	telemetryingest.RunTelemetry(s.telemetry, s.rollupDB, s.watermarks, s.layout, req.RunID, s.log)
	return engineStartResult(result, phase, attachment.Err), attachment.Err
}

// abandonReservation terminalizes a reservation whose workflow never started.
// See engine.AbandonReservation for why an un-closed reservation is worse than
// no reservation at all. Failures here are journaled to the instance log and
// otherwise swallowed: the dispatch has already failed, and the caller's error
// is the one the operator needs.
func (s *engineStarter) abandonReservation(ctx context.Context, live *livejournal.Writer, in engine.RunInput, req localscheduler.StartRequest, startedAt, finishedAt time.Time, cause error) {
	if live == nil {
		return
	}
	batch, err := engine.AbandonReservation(in, startedAt, finishedAt, cause.Error())
	if err == nil {
		_, err = live.Emit(ctx, batch)
	}
	if err != nil {
		s.echo(req, journal.Event{
			Type:   journal.EventError,
			Reason: "engine run reservation left open after a failed start",
			Error: &journal.ErrorDetail{
				Code:    "engine_reservation_abandon_failed",
				Message: err.Error(),
			},
		})
	}
}

// echo appends one instance-log record for this dispatch. Best-effort: the
// instance log is the daemon's own narrative, and failing to write it must
// not fail a run.
func (s *engineStarter) echo(req localscheduler.StartRequest, ev journal.Event) {
	if s.log == nil {
		return
	}
	ev.RunID = req.RunID
	ev.Gaggle = req.Gaggle
	ev.Workflow = s.def.Name
	if ev.Runner == nil {
		ev.Runner = map[string]any{}
	}
	ev.Runner["driver"] = string(journal.DriverEngine)
	_ = s.log.Append(ev)
}

// runnerFallbackStarter wraps the local runner's Starter for a lane the
// engine selection predicate declined, and annotates each tick with WHY.
//
// The annotation is per-tick and not per-boot on purpose. An operator
// debugging "why is this lane not on the engine?" reads the instance log for
// the run in front of them, not the daemon's startup output from three days
// ago; and a config reload can change the answer without a restart. The cost
// is one instance-log event per dispatch, which is the same order as the
// run.started the scheduler already writes.
//
// The wrapped Starter is called with the request UNMODIFIED, so a fallback
// lane behaves byte-for-byte as it did before this file existed.
type runnerFallbackStarter struct {
	next      localscheduler.Starter
	log       *journal.InstanceLog
	workflow  string
	selection engineSelection
}

func (s *runnerFallbackStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.annotate(req)
	return s.next.Start(ctx, req)
}

// Unwrap returns the wrapped Starter. It is what lets a caller — the
// scheduler's own introspection, and every test that asserts on what the
// runner path pins — reach the real starter without knowing whether this
// lane happens to be wrapped.
func (s *runnerFallbackStarter) Unwrap() localscheduler.Starter {
	if s == nil {
		return nil
	}
	return s.next
}

func (s *runnerFallbackStarter) annotate(req localscheduler.StartRequest) {
	if s.log == nil {
		return
	}
	fields := map[string]any{
		"kind":    engineStarterSelectionKind,
		"starter": "runner",
		"reason":  s.selection.FallbackReason,
	}
	if len(s.selection.SelfPinnedStages) > 0 {
		fields["selfPinnedStages"] = s.selection.SelfPinnedStages
	}
	if len(s.selection.UnpinnedGates) > 0 {
		fields["unpinnedGates"] = s.selection.UnpinnedGates
	}
	_ = s.log.Append(journal.Event{
		Type:     journal.EventRunnerAnnotation,
		RunID:    req.RunID,
		Gaggle:   req.Gaggle,
		Workflow: s.workflow,
		Reason:   "engine dispatch declined; running on the local runner",
		Runner:   fields,
	})
}

// engineStarterSelectionKind is the runner-annotation kind an operator greps
// for to see which starter served a tick.
const engineStarterSelectionKind = "engine_starter_selection"
