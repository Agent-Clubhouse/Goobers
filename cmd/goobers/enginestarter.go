package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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

func (r *engineRuntime) recoverAndAttach(ctx context.Context, layout instance.Layout, starter engine.Starter, guards *engineRunGuards, live *livejournal.Writer) error {
	if err := engine.RecoverStartIntents(ctx, filepath.Join(layout.SchedulerDir(), "engine-starts"), starter, live, func(waitCtx context.Context, in engine.RunInput) error {
		attachment := guards.await(waitCtx, in.RunID)
		if !attachment.Settled {
			return engine.ErrStartOwnershipUncertain
		}
		return nil
	}); err != nil {
		return err
	}
	r.Attach(starter, guards, live, time.Now)
	return nil
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
	starter, guards, live, now, err := s.runtime.resolve()
	if err != nil {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, err)
	}
	if req.RequireDurableJournal && (!s.liveJournal || live == nil) {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, errors.New("accepted engine dispatch requires a live journal writer before execution")
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
	spec.instanceID, err = s.layout.EnsureIdentity(ctx)
	if err != nil {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("pin engine instance identity: %w", err)
	}

	startSpec, err := engineRunSpec(spec)
	if err != nil {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, err)
	}
	in, err := engine.RunInputFor(s.def.Name, s.def, s.allowPreviewFeatures, startSpec)
	if err != nil {
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, fmt.Errorf("engine dispatch for %s/%s: %w", req.Gaggle, s.def.Name, err)
	}

	// Provisional custody survives a lost start response without claiming the
	// execution. Only a classified mine/adopt result can publish its journal.
	directory := filepath.Join(s.layout.SchedulerDir(), "engine-starts")
	var intent engine.StartIntent
	if s.liveJournal && live != nil {
		intent, err = engine.ProvisionStart(directory, in, now())
		if err != nil {
			return localscheduler.StartResult{Phase: journal.PhaseFailed}, err
		}
	}
	started, err := starter.Start(ctx, in)
	if started.NotMine {
		if intent.Input.RunID != "" {
			if err := engine.ReleaseStart(directory, in.RunID); err != nil {
				return localscheduler.StartResult{Phase: journal.PhaseFailed}, err
			}
		}
		return localscheduler.StartResult{Phase: journal.PhaseFailed}, errors.Join(engine.ErrStartNotMine, err)
	}
	if err != nil {
		// The response may have been lost after Temporal accepted the run.
		// Keep provisional custody for the boot-time reclassification pass.
		return localscheduler.StartResult{Phase: journal.PhaseFailed, FailureCode: "engine_start_failed", FailureMessage: err.Error()}, err
	}
	if intent.Input.RunID != "" {
		if err := engine.ConfirmStart(ctx, directory, intent, live); err != nil {
			return localscheduler.StartResult{Phase: journal.PhaseRunning}, err
		}
	}
	s.echo(req, journal.Event{
		Type: journal.EventRunStarted, Status: string(journal.PhaseRunning),
		Reason: "engine dispatch admitted",
	})

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
		Backprop: s.def.Spec.Backprop.EffectiveMode() == apiv1.BackpropModeActive,
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

func (s *engineStarter) RegisterDispatch() func() {
	if s.wg == nil {
		return func() {}
	}
	s.wg.Add(1)
	return s.wg.Done
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
	s.log.AppendBestEffort(ev)
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
	telemetry *telemetry.Client
	next      localscheduler.Starter
	log       *journal.InstanceLog
	workflow  string
	selection engineSelection
}

func (s *runnerFallbackStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.annotate(req)
	s.observeFallback(ctx, req)
	return s.next.Start(ctx, req)
}

func (s *runnerFallbackStarter) RegisterDispatch() func() {
	if registered, ok := s.next.(localscheduler.DispatchRegistration); ok {
		return registered.RegisterDispatch()
	}
	return func() {}
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
	fields := s.selection.annotationFields()
	s.log.AppendBestEffort(journal.Event{
		Type: journal.EventRunnerAnnotation, RunID: req.RunID,
		Gaggle: req.Gaggle, Workflow: s.workflow,
		Reason: "engine dispatch declined; running on the local runner", Runner: fields,
	})
}

func (selection engineSelection) annotationFields() map[string]any {
	fields := map[string]any{
		"kind":              engineStarterSelectionKind,
		"starter":           "runner",
		"reason":            selection.FallbackReason,
		"reasonClass":       selection.ReasonClass,
		"placementDeclared": selection.PlacementDeclared,
	}
	if len(selection.SelfPinnedStages) > 0 {
		fields["selfPinnedStages"] = selection.SelfPinnedStages
	}
	if len(selection.UnpinnedGates) > 0 {
		fields["unpinnedGates"] = selection.UnpinnedGates
	}
	return fields
}

// engineStarterSelectionKind is shared with the run and workflow projections.
const engineStarterSelectionKind = journal.RunnerAnnotationEngineSelection
