package localscheduler

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// WorkflowEntry is one workflow the scheduler manages: its readiness
// conditions, its schedule triggers (empty for a manual/signal-only
// workflow; backlog-item consumption is itself a cron-triggered workflow
// whose first stage claims items), the external signal names it's
// subscribed to (#342 — type=signal triggers), the Starter that dispatches
// a run, and the repo every run's stages branch worktrees from. A workflow
// may declare more than one schedule trigger (#341) — Tick fires if any of
// them is due, sharing one LastEval baseline per workflow rather than
// tracking each schedule independently.
type WorkflowEntry struct {
	Workflow  string
	Gaggle    string
	Readiness apiv1.ReadinessConditions
	Schedules []Schedule
	Signals   []string
	Starter   Starter
	RepoRef   apiv1.RepoRef
}

// minPoll floors the computed sleep-until-next-tick duration, so a schedule
// that just fired (Next() a few nanoseconds out due to clock jitter) can't spin
// the loop.
const minPoll = time.Second

// newRunID is the run-id generator; swappable in tests for determinism.
var newRunID = telemetry.NewRunID

// SpanStarter is the slice of the telemetry client the local scheduler needs
// to open a decision span per dispatch (issue #126). *telemetry.Client
// satisfies it structurally, mirroring internal/scheduler.SpanStarter's
// narrow-interface pattern for the tier-3 scheduler.
type SpanStarter interface {
	StartSchedulerSpan(ctx context.Context, attrs telemetry.SchedulerAttributes) (context.Context, telemetry.Span, error)
}

// Scheduler is the embedded scheduler daemon (§7, SCH-001): it ties cron
// evaluation, run conditions, and the Starter seam together into one
// idle-between-ticks loop, journaling every decision to the instance journal.
type Scheduler struct {
	workflows  map[string]WorkflowEntry
	conditions *Conditions
	log        *journal.InstanceLog
	now        func() time.Time
	after      func(d time.Duration) <-chan time.Time
	telemetry  SpanStarter

	mu       sync.Mutex
	triggers map[string]TriggerState
}

// Option configures a Scheduler.
type Option func(*Scheduler)

// WithClock overrides the time source and the timer primitive (for
// deterministic, non-busy-waiting tests). Defaults to time.Now/time.After.
func WithClock(now func() time.Time, after func(time.Duration) <-chan time.Time) Option {
	return func(s *Scheduler) {
		s.now = now
		s.after = after
	}
}

// WithTelemetry records a scheduler decision span per dispatch (issue #126).
// Optional — nil (the default) emits no spans.
func WithTelemetry(t SpanStarter) Option {
	return func(s *Scheduler) {
		s.telemetry = t
	}
}

// WithInstanceRunConditions applies instance.yaml's runConditions (§7,
// SCH-003's "max-parallel per workflow/instance") on top of each workflow's
// own per-workflow conditions — before this option existed, instance.yaml's
// maxParallelRuns/workflowBudgets were parsed and scaffolded but enforced
// nowhere (issue #142). maxParallelRuns caps total concurrent runs across
// every workflow in the instance (0/unset = unlimited); workflowBudgets
// overrides a named workflow's runs-per-hour budget; dayBudgets overrides a
// named workflow's runs-per-day budget (#340).
func WithInstanceRunConditions(maxParallelRuns int, workflowBudgets map[string]int, dayBudgets map[string]int) Option {
	return func(s *Scheduler) {
		s.conditions.SetInstanceLimits(maxParallelRuns, workflowBudgets, dayBudgets)
	}
}

// New builds a Scheduler over the given workflow entries. Call Reconcile
// before Run to seed run-condition and trigger state from durable state after
// a restart; a freshly-created instance can skip it (everything starts empty).
func New(entries []WorkflowEntry, log *journal.InstanceLog, opts ...Option) *Scheduler {
	s := &Scheduler{
		workflows:  make(map[string]WorkflowEntry, len(entries)),
		conditions: NewConditions(),
		log:        log,
		now:        time.Now,
		after:      time.After,
		triggers:   make(map[string]TriggerState),
	}
	for _, opt := range opts {
		opt(s)
	}
	for _, e := range entries {
		s.workflows[e.Workflow] = e
		ts := TriggerState{Workflow: e.Workflow, Schedules: e.Schedules, LastEval: s.now()}
		s.triggers[e.Workflow] = ts
	}
	return s
}

// Reconcile seeds Conditions' active-run counts and rolling budget window,
// and each workflow's trigger LastEval, from durable state — the
// daemon-restart recovery pass. Call once before Run.
//
// The active-run counts this seeds are a starting point, not a
// self-releasing snapshot: whatever the caller does with those pre-existing
// non-terminal runs (issue #135's daemon-startup recovery, e.g.
// Runner.Resume) MUST call Release once each one's outcome is known, the
// same reserve-then-Release contract Admit's own callers follow — otherwise
// the seeded count never comes back down and the workflow starves for the
// rest of the daemon's life. See Release below.
func (s *Scheduler) Reconcile(runsDir string, now time.Time) error {
	active, err := ActiveRunCounts(runsDir)
	if err != nil {
		return fmt.Errorf("localscheduler: reconcile active runs: %w", err)
	}
	s.conditions.Reconcile(active)

	events, err := journal.ReadInstanceLog(s.log.Dir())
	if err != nil {
		return fmt.Errorf("localscheduler: reconcile trigger history: %w", err)
	}
	var fired []TriggerFiredRecord
	starts := map[string][]time.Time{}
	// dayWindow, not budgetWindow (#340): Conditions retains one starts
	// history per workflow at dayWindow width to serve both the hourly and
	// the daily budget check, so the history seeded here after a restart
	// must be at least as wide or the daily check would under-count.
	startsCutoff := now.Add(-dayWindow)
	// A narrow rate-limit reset (#315: `goobers reset-rate-limit`) raises the
	// window floor to the reset moment: run.started events at or before it stop
	// counting toward MaxRunsPerHour (or MaxRunsPerDay), so an operator can
	// "run again now" without the old `rm -rf <instance>` workaround that also
	// destroyed runs/ (the durable run journals). It only ever moves the floor
	// forward — a reset older than the rolling window is a natural no-op,
	// since the window has already advanced past it.
	if resetAt, ok, rerr := ReadRateReset(s.log.Dir()); rerr != nil {
		return fmt.Errorf("localscheduler: read rate-limit reset: %w", rerr)
	} else if ok && resetAt.After(startsCutoff) {
		startsCutoff = resetAt
	}
	for _, ev := range events {
		if ev.Type == journal.EventTriggerFired {
			fired = append(fired, TriggerFiredRecord{Workflow: ev.Workflow, Time: ev.Time})
		}
		if ev.Type == journal.EventRunStarted && ev.Time.After(startsCutoff) {
			starts[ev.Workflow] = append(starts[ev.Workflow], ev.Time)
		}
	}
	s.conditions.ReconcileBudget(starts)

	names := make([]string, 0, len(s.workflows))
	for name := range s.workflows {
		names = append(names, name)
	}
	last := ReconstructLastEval(fired, names, now)

	s.mu.Lock()
	defer s.mu.Unlock()
	for name, at := range last {
		ts := s.triggers[name]
		ts.LastEval = at
		s.triggers[name] = ts
	}
	return nil
}

// Release returns a workflow's held concurrency slot. Exported so a caller
// resuming interrupted runs outside the normal Tick/Trigger dispatch path
// (the daemon's startup recovery scan, issue #135) can release the slot
// Reconcile seeded for it once that resumed run's outcome is known, exactly
// as dispatch's own goroutine does for a newly-dispatched run — without
// this, a reconciled slot for a run that predates this process has no
// release path at all and starves its workflow for the daemon's lifetime.
func (s *Scheduler) Release(workflow string) {
	s.conditions.Release(workflow)
}

// Run is the daemon loop: evaluate every workflow's trigger, dispatch what's
// due and admitted, then idle until the next tick is worth taking — no
// busy-polling, per the acceptance criterion. It returns when ctx is
// cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		s.Tick(ctx, s.now())

		wait := s.nextWakeup(s.now())
		if wait < minPoll {
			wait = minPoll
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.after(wait):
		}
	}
}

// Tick evaluates every workflow's trigger at now and dispatches what's due and
// admitted. Exported (not just used by Run's loop) so a manual `goobers run
// <workflow>` trigger and tests can drive a single evaluation deterministically
// without running the full timer loop.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	entries := make([]WorkflowEntry, 0, len(s.workflows))
	for _, e := range s.workflows {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	for _, entry := range entries {
		if len(entry.Schedules) == 0 {
			continue // manual-only workflow: not cron-managed
		}
		// Read, evaluate, and write the trigger state under a single lock
		// acquisition. Tick is exported so a manual trigger and concurrent
		// Tick calls (e.g. overlapping Run-loop iterations) can race here;
		// dropping the lock between the read and the write let two callers
		// both read the same pre-fire TriggerState, both compute Fire=true,
		// and both dispatch the same due firing.
		s.mu.Lock()
		ts := s.triggers[entry.Workflow]
		res := Tick(ts, now)
		s.triggers[entry.Workflow] = TriggerState{Workflow: entry.Workflow, Schedules: entry.Schedules, LastEval: res.LastEval}
		s.mu.Unlock()
		if !res.Fire {
			continue
		}
		s.dispatch(ctx, entry, now, res, journal.TriggerSchedule)
	}
}

// Trigger manually fires workflow now, bypassing its cron schedule but still
// honoring run conditions (SCH-002; `goobers run <workflow>` CLI wiring calls
// this — issue #134). Returns the dispatched run's id once conditions admit
// it — before the run itself completes, since dispatch always continues
// asynchronously (see dispatch's goroutine) — so a caller that wants to
// observe the run to completion polls that id's own journal, the same way
// `goobers status`/`trace` do. Returns an error if the workflow is unknown or
// run conditions rejected the trigger (a conditions-driven skip is NOT a
// silent no-op here, unlike a cron Tick's skip, since a human explicitly
// asked for this run and deserves to know why it didn't start).
func (s *Scheduler) Trigger(ctx context.Context, workflow string, now time.Time) (runID string, err error) {
	s.mu.Lock()
	entry, ok := s.workflows[workflow]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("localscheduler: unknown workflow %q", workflow)
	}
	runID, admitted, skipReason := s.dispatch(ctx, entry, now, TickResult{Fire: true, LastEval: now}, journal.TriggerManual)
	if !admitted {
		return "", fmt.Errorf("localscheduler: run conditions rejected the trigger for %q: %s", workflow, skipReason)
	}
	return runID, nil
}

// Signal fires every workflow subscribed to the named external signal (WF-014,
// #342: a type=signal trigger declares Signal: "<name>") — `goobers signal
// <name>` CLI wiring calls this today; an HTTP/webhook sink (#169) is the
// planned future caller, once the daemon has a write-capable API surface, but
// this method itself has no delivery-mechanism opinion. Unlike Trigger (which
// names exactly one workflow and reports why it didn't start), Signal
// broadcasts to however many workflows are subscribed — zero, one, or many —
// so a conditions-driven skip is silent per subscriber (best-effort, the same
// semantics a cron Tick's skip has) rather than a caller-facing error; the
// skip is still journaled via dispatch's own tick.skipped event. Returns the
// run ids of every workflow actually admitted, in workflow-name order for
// determinism.
func (s *Scheduler) Signal(ctx context.Context, name string, now time.Time) []string {
	s.mu.Lock()
	var subscribed []WorkflowEntry
	for _, e := range s.workflows {
		for _, sig := range e.Signals {
			if sig == name {
				subscribed = append(subscribed, e)
				break
			}
		}
	}
	s.mu.Unlock()
	sort.Slice(subscribed, func(i, j int) bool { return subscribed[i].Workflow < subscribed[j].Workflow })

	var runIDs []string
	for _, entry := range subscribed {
		if runID, admitted, _ := s.dispatch(ctx, entry, now, TickResult{Fire: true, LastEval: now}, journal.TriggerSignal); admitted {
			runIDs = append(runIDs, runID)
		}
	}
	return runIDs
}

// dispatch admits and starts (or skips) one due firing of entry. kind tags
// both the trigger.fired reason (journal.TriggerManual renders as "manual",
// never "scheduled" — issue #134's fireReason mislabeling fix) and the
// dispatched run's own journal.Trigger.Kind, previously hardcoded to
// TriggerSchedule even for a manual Scheduler.Trigger call. Returns the
// dispatched run's id (empty if skipped), whether it was admitted, and — when
// not admitted — the run-conditions skip reason, so Trigger can surface it to
// a human caller instead of silently doing nothing.
//
// The telemetry span it opens covers only the decision (trigger -> admit/skip
// -> run-id mint), not the run itself: entry.Starter.Start runs in its own
// goroutine below and outlives dispatch's return, so the run gets its own
// root span (via runner.Runner.startRunSpan) on its own trace rather than a
// child of a span that already ended — same rationale as
// internal/scheduler.Scheduler.startSpan omitting RunID.
func (s *Scheduler) dispatch(ctx context.Context, entry WorkflowEntry, now time.Time, tick TickResult, kind journal.TriggerKind) (runID string, admitted bool, skipReason string) {
	span := s.startSpan(ctx, entry, tick, kind)
	defer span.End()

	// Unlike the journalEvent calls below (best-effort: they record a
	// decision already made, so a write failure doesn't roll it back), a
	// failed trigger.fired append MUST stop this dispatch here rather than
	// being swallowed (SCH-031, issue #142): ReconstructLastEval rebuilds
	// each workflow's LastEval purely from trigger.fired history after a
	// restart, so a fire that started a run but never durably recorded
	// having fired would replay on the very next restart — dispatching a
	// second run for the same nominal firing. Refusing to dispatch keeps the
	// invariant that a run only ever starts once its trigger.fired record
	// has durably landed.
	if err := s.appendJournalEvent(journal.Event{
		Type:     journal.EventTriggerFired,
		Workflow: entry.Workflow,
		Reason:   fireReason(tick, kind),
	}); err != nil {
		reason := "trigger.fired journal write failed: " + err.Error()
		span.Fail(err)
		return "", false, reason
	}

	ok, reason := s.conditions.Admit(entry.Workflow, entry.Readiness, now)
	if !ok {
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Reason:   reason,
		})
		span.Succeed("skipped: " + reason)
		return "", false, reason
	}

	runID, err := newRunID()
	if err != nil {
		s.conditions.Release(entry.Workflow)
		reason := "run-id generation failed: " + err.Error()
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Reason:   reason,
		})
		span.Fail(err)
		return "", false, reason
	}

	s.journalEvent(journal.Event{
		Type:     journal.EventRunStarted,
		Workflow: entry.Workflow,
		RunID:    runID,
	})
	span.Succeed("started: " + runID)

	go func() {
		defer s.conditions.Release(entry.Workflow)
		result, startErr := entry.Starter.Start(ctx, StartRequest{
			RunID:   runID,
			Gaggle:  entry.Gaggle,
			Trigger: journal.Trigger{Kind: kind, Ref: entry.Workflow},
			RepoRef: entry.RepoRef,
		})
		status := string(result.Phase)
		if startErr != nil {
			status = "error: " + startErr.Error()
		}
		s.journalEvent(journal.Event{
			Type:     journal.EventRunFinished,
			Workflow: entry.Workflow,
			RunID:    runID,
			Status:   status,
		})
	}()
	return runID, true, ""
}

// startSpan opens a scheduler decision span for entry's dispatch, if
// telemetry is configured. A zero telemetry.Span is safe to use (its methods
// no-op), so callers need no nil checks.
func (s *Scheduler) startSpan(ctx context.Context, entry WorkflowEntry, tick TickResult, kind journal.TriggerKind) telemetry.Span {
	if s.telemetry == nil {
		return telemetry.Span{}
	}
	attrs := telemetry.SchedulerAttributes{
		Gaggle:     entry.Gaggle,
		WorkflowID: entry.Workflow,
		Action:     "dispatch",
		Reason:     fireReason(tick, kind),
	}
	_, span, err := s.telemetry.StartSchedulerSpan(ctx, attrs)
	if err != nil {
		return telemetry.Span{}
	}
	return span
}

// fireReason renders a stable reason string for a trigger.fired event. A
// manual trigger (issue #23's `goobers run`/#134) always renders "manual"
// and a signal (#342's `Signal`/`goobers signal`) always renders "signal",
// both distinct from a cron tick's "scheduled"/"catch-up (missed N)" —
// neither has a TickResult.CatchUp concept of its own, so kind takes
// priority over it.
func fireReason(tick TickResult, kind journal.TriggerKind) string {
	switch kind {
	case journal.TriggerManual:
		return "manual"
	case journal.TriggerSignal:
		return "signal"
	}
	if tick.CatchUp {
		return fmt.Sprintf("catch-up (missed %d)", tick.MissedTicks)
	}
	return "scheduled"
}

// nextWakeup computes how long to sleep until the earliest workflow trigger is
// next due, so Run idles instead of busy-polling. Workflows with no schedule
// (manual-only) don't contribute; if none are cron-managed, it returns a
// conservative default so the loop still wakes periodically for Reconcile-style
// housekeeping rather than blocking forever.
func (s *Scheduler) nextWakeup(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	var earliest time.Time
	for name, entry := range s.workflows {
		if len(entry.Schedules) == 0 {
			continue
		}
		ts := s.triggers[name]
		for _, sched := range entry.Schedules {
			next := sched.Next(ts.LastEval)
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
			}
		}
	}
	if earliest.IsZero() {
		return time.Minute
	}
	if d := earliest.Sub(now); d > 0 {
		return d
	}
	return minPoll
}

// journalEvent appends to the instance journal if one is wired; best-effort,
// same rationale as ClaimLedger.journal — a journal write failure doesn't roll
// back a scheduling decision already made.
func (s *Scheduler) journalEvent(ev journal.Event) {
	_ = s.appendJournalEvent(ev)
}

// appendJournalEvent appends to the instance journal if one is wired,
// returning the write error to the (rare) caller that must act on it —
// dispatch's trigger.fired append, see its own comment for why. A nil log is
// not an error (many tests construct a Scheduler with none).
func (s *Scheduler) appendJournalEvent(ev journal.Event) error {
	if s.log == nil {
		return nil
	}
	return s.log.Append(ev)
}
