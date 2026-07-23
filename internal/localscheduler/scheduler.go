package localscheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/internal/telemetry"
)

// WorkflowEntry is one workflow the scheduler manages: its readiness
// conditions, its schedule triggers (empty for a manual/signal/backlog-item-
// only workflow), the external signal names it's subscribed to (#342 —
// type=signal triggers), an optional BacklogCounter for a type=backlog-item
// trigger (#344), the Starter that dispatches a run, and the repo every
// run's stages branch worktrees from. A workflow may declare more than one
// schedule trigger (#341) — Tick fires if any of them is due, sharing one
// LastEval baseline per workflow rather than tracking each schedule
// independently. Schedules, Signals, and BacklogCounter are independent —
// Tick/signal delivery evaluates whichever are set, since nothing prevents
// a workflow from declaring more than one trigger type.
type WorkflowEntry struct {
	Workflow        string
	WorkflowVersion int
	WorkflowDigest  string
	GooberDigest    string
	Gaggle          string
	Readiness       apiv1.ReadinessConditions
	Schedules       []Schedule
	Signals         []string
	// BacklogCounter, when set, marks this workflow as backlog-item-triggered
	// (#344): Tick polls it every backlogPollInterval instead of (or in
	// addition to, if Schedules is also set) evaluating a cron schedule, and
	// fans out up to that many runs at once — unlike a schedule trigger's
	// fixed one-shot-per-fire model, a backlog-item trigger starts as many
	// runs as there are ready items, bounded by run conditions.
	BacklogCounter BacklogCounter
	Starter        Starter
	RepoRef        apiv1.RepoRef
	// RequiredCapabilities is the union of runner (toolchain/platform)
	// capabilities this workflow's gaggle and stages require (RRQ-1/#1101).
	// dispatch refuses the run before admission when the runner does not claim
	// every entry. Nil/empty imposes no requirement.
	RequiredCapabilities []string
}

func entryIdentity(entry WorkflowEntry) WorkflowIdentity {
	return WorkflowIdentity{Gaggle: entry.Gaggle, Workflow: entry.Workflow}
}

// BacklogCounter reports how many eligible backlog items are ready for a
// workflow whose trigger is type=backlog-item (#344). Tick calls this once
// per backlogPollInterval instead of evaluating a cron Schedule, then
// dispatches up to that many runs in the same evaluation (each still gated
// by the ordinary run-conditions Admit check) — turning "one trigger firing
// = at most one new run, always" (the bug #344 reports) into fan-out sized
// to actual backlog readiness.
type BacklogCounter interface {
	EligibleCount(ctx context.Context) (int, error)
}

// minPoll floors the computed sleep-until-next-tick duration, so a schedule
// that just fired (Next() a few nanoseconds out due to clock jitter) can't spin
// the loop.
const minPoll = time.Second

// backlogPollInterval bounds how often a backlog-item-triggered workflow's
// BacklogCounter is polled (#344) — a real provider call (ListWorkItems),
// unlike a schedule trigger's free in-memory Next() check, so this must not
// run on every minPoll-floored loop iteration the way cron evaluation does;
// 30s balances promptness (a fan-out opportunity is noticed soon) against
// API-rate-limit and log-noise cost of polling every ready backlog item's
// count that often.
const backlogPollInterval = 30 * time.Second

const starvationSkipThreshold = 3

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
	workflows         map[WorkflowIdentity]WorkflowEntry
	conditions        *Conditions
	log               *journal.InstanceLog
	now               func() time.Time
	after             func(d time.Duration) <-chan time.Time
	telemetry         SpanStarter
	afterTick         func(context.Context)
	heartbeatInterval time.Duration
	refreshHeartbeat  func(time.Time) error
	writeTriggerState func(string, map[WorkflowIdentity]time.Time) error

	mu         sync.Mutex
	tickMu     sync.Mutex
	triggers   map[WorkflowIdentity]TriggerState
	dispatches sync.WaitGroup
	wake       chan struct{}
	// reconciledRuns identifies the pre-existing runs represented in
	// Conditions' startup counts, so recovery releases cannot consume another
	// run's workflow-level slot.
	reconciledRuns map[string]WorkflowIdentity
	// admittedRuns identifies live dispatches whose condition slots remain
	// reserved until their Starter returns or a watchdog terminalizes them.
	admittedRuns map[string]WorkflowIdentity
	// backlogLastCheck tracks, per backlog-item-triggered workflow, when its
	// BacklogCounter was last polled (#344) — separate from triggers'
	// LastEval, which is cron-Schedule-specific bookkeeping a workflow with
	// both trigger kinds must not have corrupted by backlog-check timing.
	// Reset to empty on every restart (not reconciled from durable history):
	// the worst case is one extra poll right after a restart, not a
	// correctness bug, so it isn't worth the added Reconcile complexity.
	backlogLastCheck map[WorkflowIdentity]time.Time
	// consecutivePoolSkips ages workflows that were due and otherwise ready
	// but could not enter the shared instance concurrency pool.
	consecutivePoolSkips map[WorkflowIdentity]int
	// lastDispatchedGaggle is the cursor for work-conserving round-robin
	// dispatch across gaggles. hasDispatchedGaggle distinguishes the initial
	// state from a legacy single-gaggle entry whose gaggle name is empty.
	lastDispatchedGaggle string
	hasDispatchedGaggle  bool
	// runnerCapabilities is the local runner's static advertised capability set
	// (RRQ-1/#1101). Set once at construction, read-only thereafter, so it needs
	// no lock. A dispatch is refused before admission when the entry requires a
	// capability the runner does not claim. Empty (the default) claims nothing,
	// which only matters for entries that declare RequiredCapabilities — an
	// entry that declares none is never refused on this axis.
	runnerCapabilities runnercap.Claimed
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

// WithAfterTick registers work that runs after each trigger evaluation, once
// all scheduler decision spans opened by that tick have ended.
func WithAfterTick(afterTick func(context.Context)) Option {
	return func(s *Scheduler) {
		s.afterTick = afterTick
	}
}

// WithTickHeartbeat records completed daemon ticks and bounds idle waits so
// the heartbeat remains fresh even when the next workflow trigger is far away.
func WithTickHeartbeat(interval time.Duration, refresh func(time.Time) error) Option {
	if interval <= 0 {
		panic("scheduler heartbeat interval must be positive")
	}
	if refresh == nil {
		panic("scheduler heartbeat refresh function is required")
	}
	return func(s *Scheduler) {
		s.heartbeatInterval = interval
		s.refreshHeartbeat = refresh
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

// WithOpenPRCounter wires the cached open-PR counter that backs the MaxOpenPRs
// readiness cap (#353). Optional — nil/unset leaves the cap unenforced, so a
// workflow that sets MaxOpenPRs without a counter wired simply isn't throttled.
func WithOpenPRCounter(counter OpenPRCounter) Option {
	return func(s *Scheduler) {
		if counter != nil {
			s.conditions.SetOpenPRCounter(counter)
		}
	}
}

// WithProviderQuota wires the gate that backs the provider-quota circuit
// breaker (#712). Optional — nil/unset leaves the breaker unenforced.
func WithProviderQuota(gate ProviderQuotaGate) Option {
	return func(s *Scheduler) {
		if gate != nil {
			s.conditions.SetProviderQuota(gate)
		}
	}
}

// WithRunnerCapabilities declares the local runner's static advertised
// capability set (RRQ-1/#1101). A dispatch whose entry requires a capability
// not in this set is refused before admission, journaling a tick.skipped with a
// ReasonMissingCapability diagnostic naming the gap. Optional — unset claims
// nothing, so only entries that declare RequiredCapabilities are ever affected.
func WithRunnerCapabilities(caps []string) Option {
	return func(s *Scheduler) {
		s.runnerCapabilities = runnercap.NewClaimed(caps)
	}
}

// New builds a Scheduler over the given workflow entries. Call Reconcile
// before Run to seed run-condition and trigger state from durable state after
// a restart; a freshly-created instance can skip it (everything starts empty).
func New(entries []WorkflowEntry, log *journal.InstanceLog, opts ...Option) *Scheduler {
	s := &Scheduler{
		workflows:            make(map[WorkflowIdentity]WorkflowEntry, len(entries)),
		conditions:           NewConditions(),
		log:                  log,
		now:                  time.Now,
		after:                time.After,
		triggers:             make(map[WorkflowIdentity]TriggerState),
		reconciledRuns:       make(map[string]WorkflowIdentity),
		admittedRuns:         make(map[string]WorkflowIdentity),
		backlogLastCheck:     make(map[WorkflowIdentity]time.Time),
		consecutivePoolSkips: make(map[WorkflowIdentity]int),
		wake:                 make(chan struct{}, 1),
		writeTriggerState:    writeTriggerEvaluations,
	}
	for _, opt := range opts {
		opt(s)
	}
	for _, e := range entries {
		identity := entryIdentity(e)
		s.workflows[identity] = e
		ts := TriggerState{Workflow: e.Workflow, Schedules: e.Schedules, LastEval: s.now()}
		s.triggers[identity] = ts
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
// Runner.Resume) MUST call ReleaseReconciled once each one's outcome is known,
// the same reserve-then-release contract Admit's own callers follow —
// otherwise the seeded count never comes back down and the workflow starves
// for the rest of the daemon's life.
func (s *Scheduler) Reconcile(runsDir string, now time.Time) error {
	return s.ReconcileAll([]string{runsDir}, now)
}

// ReconcileAll reconciles durable state across all per-gaggle run roots.
func (s *Scheduler) ReconcileAll(runsDirs []string, now time.Time) error {
	active, runs, err := activeRuns(runsDirs)
	if err != nil {
		return fmt.Errorf("localscheduler: reconcile active runs: %w", err)
	}
	s.conditions.ReconcileWorkflows(active)
	s.mu.Lock()
	s.reconciledRuns = runs
	s.mu.Unlock()

	events, err := journal.ReadInstanceLog(s.log.Dir())
	if err != nil {
		return fmt.Errorf("localscheduler: reconcile trigger history: %w", err)
	}
	var fired []TriggerFiredRecord
	starts := map[WorkflowIdentity][]time.Time{}
	identities := make([]WorkflowIdentity, 0, len(s.workflows))
	for identity := range s.workflows {
		identities = append(identities, identity)
	}
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
		if ev.Type == journal.EventTriggerFired && scheduledTriggerFired(ev.Reason) {
			fired = append(fired, TriggerFiredRecord{Gaggle: ev.Gaggle, Workflow: ev.Workflow, Time: ev.Time})
		}
		if ev.Type == journal.EventRunStarted && ev.Time.After(startsCutoff) {
			for _, identity := range resolveRunStartedIdentities(runsDirs, ev, identities) {
				starts[identity] = append(starts[identity], ev.Time)
			}
		}
	}
	s.conditions.ReconcileWorkflowBudgets(starts)
	last := ReconstructLastEval(fired, identities, now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistTriggerEvaluationsLocked(last); err != nil {
		return err
	}
	for identity := range s.triggers {
		ts := s.triggers[identity]
		at := last[identity]
		ts.LastEval = at
		s.triggers[identity] = ts
	}
	return nil
}

func resolveRunStartedIdentities(runsDirs []string, event journal.Event, workflows []WorkflowIdentity) []WorkflowIdentity {
	if identity, ok := resolveWorkflowIdentity(event.Gaggle, event.Workflow, workflows); ok {
		return []WorkflowIdentity{identity}
	}
	if event.Gaggle != "" {
		return nil
	}

	if apiv1.ValidRunID(event.RunID) {
		for _, runsDir := range runsDirs {
			reader, err := journal.OpenRead(filepath.Join(runsDir, event.RunID))
			if err == nil {
				run, err := reader.Identity()
				if err == nil && run.RunID == event.RunID && run.Workflow == event.Workflow {
					if identity, ok := resolveWorkflowIdentity(run.Gaggle, run.Workflow, workflows); ok {
						return []WorkflowIdentity{identity}
					}
				}
			}
		}
	}

	// Legacy instance events did not record gaggle. If their run journal is no
	// longer available, retain the budget against every matching workflow rather
	// than resetting admission history after a same-named workflow is added.
	matches := make([]WorkflowIdentity, 0)
	for _, identity := range workflows {
		if identity.Workflow == event.Workflow {
			matches = append(matches, identity)
		}
	}
	return matches
}

// ReleaseReconciled returns the slot Reconcile seeded for runID, if any.
// Matching by run prevents terminal cleanup from consuming another running
// run's workflow-level slot when no slot was seeded for the terminal run.
func (s *Scheduler) ReleaseReconciled(runID, workflow string) {
	s.mu.Lock()
	reconciledWorkflow, ok := s.reconciledRuns[runID]
	if ok && reconciledWorkflow.Workflow == workflow {
		delete(s.reconciledRuns, runID)
	}
	s.mu.Unlock()
	if ok && reconciledWorkflow.Workflow == workflow {
		s.conditions.ReleaseWorkflow(reconciledWorkflow)
	}
}

// ReleaseRun returns a run's live admission or restart-reconciled slot exactly
// once. Watchdogs use this after terminalizing a run; dispatch and resume
// cleanup may safely call it again.
func (s *Scheduler) ReleaseRun(runID, workflow string) {
	s.mu.Lock()
	identity, admitted := s.admittedRuns[runID]
	if admitted && identity.Workflow == workflow {
		delete(s.admittedRuns, runID)
	}
	reconciledIdentity, reconciled := s.reconciledRuns[runID]
	if reconciled && reconciledIdentity.Workflow == workflow {
		delete(s.reconciledRuns, runID)
	}
	s.mu.Unlock()

	switch {
	case admitted && identity.Workflow == workflow:
		s.conditions.ReleaseWorkflow(identity)
	case reconciled && reconciledIdentity.Workflow == workflow:
		s.conditions.ReleaseWorkflow(reconciledIdentity)
	}
}

// Wait blocks until every admitted dispatch has finished its Starter call and
// post-run bookkeeping. Callers must stop initiating dispatches before waiting.
func (s *Scheduler) Wait() {
	s.dispatches.Wait()
}

// Run is the daemon loop: evaluate every workflow's trigger, dispatch what's
// due and admitted, then idle until the next tick is worth taking — no
// busy-polling, per the acceptance criterion. It returns when ctx is
// cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		s.Tick(ctx, s.now())
		if s.refreshHeartbeat != nil {
			if err := s.refreshHeartbeat(s.now()); err != nil {
				return fmt.Errorf("refresh scheduler heartbeat: %w", err)
			}
		}

		wait := s.nextWakeup(s.now())
		if s.heartbeatInterval > 0 && wait > s.heartbeatInterval {
			wait = s.heartbeatInterval
		}
		if wait < minPoll {
			wait = minPoll
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
		case <-s.after(wait):
		}
	}
}

type tickCandidate struct {
	entry              WorkflowEntry
	schedule           TickResult
	scheduleDue        bool
	backlogRemaining   int
	poolSkips          int
	dispatchedThisTick bool
	stopped            bool
}

func (c *tickCandidate) next() (TickResult, journal.TriggerKind, bool) {
	if c.scheduleDue {
		c.scheduleDue = false
		return c.schedule, journal.TriggerSchedule, true
	}
	if c.backlogRemaining > 0 {
		c.backlogRemaining--
		return TickResult{Fire: true, LastEval: c.schedule.LastEval}, journal.TriggerItem, true
	}
	return TickResult{}, "", false
}

type tickGaggle struct {
	candidates []*tickCandidate
	nextIndex  int
}

func (g *tickGaggle) next() (*tickCandidate, TickResult, journal.TriggerKind, bool) {
	for range len(g.candidates) {
		candidate := g.candidates[g.nextIndex]
		g.nextIndex = (g.nextIndex + 1) % len(g.candidates)
		if candidate.stopped {
			continue
		}
		tick, kind, ok := candidate.next()
		if ok {
			return candidate, tick, kind, true
		}
	}
	return nil, TickResult{}, "", false
}

// Tick evaluates every workflow's trigger at now, orders due workflows by
// starvation age within each gaggle, and dispatches one item per ready gaggle
// per pass until demand or capacity is exhausted. The gaggle order resumes
// after the most recently admitted gaggle. With G continuously ready gaggles,
// this bounds a gaggle's wait to G-1 successful dispatches by other gaggles.
// Gaggles without ready work are omitted, so they never reserve capacity.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()

	s.mu.Lock()
	entries := make([]WorkflowEntry, 0, len(s.workflows))
	for _, e := range s.workflows {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	candidates := make([]*tickCandidate, 0, len(entries))
	for _, entry := range entries {
		identity := entryIdentity(entry)
		candidate := &tickCandidate{
			entry:    entry,
			schedule: TickResult{LastEval: now},
		}
		if len(entry.Schedules) > 0 {
			// Read, evaluate, and write the trigger state under a single lock
			// acquisition. Tick is exported so a manual trigger and concurrent
			// Tick calls (e.g. overlapping Run-loop iterations) can race here;
			// dropping the lock between the read and the write let two callers
			// both read the same pre-fire TriggerState, both compute Fire=true,
			// and both dispatch the same due firing.
			s.mu.Lock()
			ts := s.triggers[identity]
			res := Tick(ts, now)
			var persistErr error
			if res.LastEval != ts.LastEval {
				evaluations := s.triggerEvaluationsLocked()
				evaluations[identity] = res.LastEval
				persistErr = s.persistTriggerEvaluationsLocked(evaluations)
			}
			s.triggers[identity] = TriggerState{Workflow: entry.Workflow, Schedules: entry.Schedules, LastEval: res.LastEval}
			s.mu.Unlock()
			if persistErr != nil {
				s.journalEvent(journal.Event{
					Type:     journal.EventError,
					Workflow: entry.Workflow,
					Gaggle:   entry.Gaggle,
					Error:    &journal.ErrorDetail{Code: "trigger_state_persist_failed", Message: persistErr.Error()},
				})
			}
			if res.Fire {
				candidate.schedule = res
				candidate.scheduleDue = true
			}
		}

		if entry.BacklogCounter != nil {
			candidate.backlogRemaining = s.pollBacklog(ctx, entry, now)
		}
		if candidate.scheduleDue || candidate.backlogRemaining > 0 {
			s.mu.Lock()
			candidate.poolSkips = s.consecutivePoolSkips[identity]
			s.mu.Unlock()
			candidates = append(candidates, candidate)
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].poolSkips != candidates[j].poolSkips {
			return candidates[i].poolSkips > candidates[j].poolSkips
		}
		if candidates[i].entry.Workflow != candidates[j].entry.Workflow {
			return candidates[i].entry.Workflow < candidates[j].entry.Workflow
		}
		return candidates[i].entry.Gaggle < candidates[j].entry.Gaggle
	})

	byGaggle := make(map[string]*tickGaggle)
	gaggleNames := make([]string, 0)
	for _, candidate := range candidates {
		gaggle := candidate.entry.Gaggle
		group, ok := byGaggle[gaggle]
		if !ok {
			group = &tickGaggle{}
			byGaggle[gaggle] = group
			gaggleNames = append(gaggleNames, gaggle)
		}
		group.candidates = append(group.candidates, candidate)
	}
	gaggleNames = s.orderedGaggles(gaggleNames)
	gaggles := make([]*tickGaggle, 0, len(gaggleNames))
	for _, name := range gaggleNames {
		gaggles = append(gaggles, byGaggle[name])
	}

	for {
		attempted := false
		for _, gaggle := range gaggles {
			for {
				candidate, tick, kind, ok := gaggle.next()
				if !ok {
					break
				}
				attempted = true
				_, admitted, reason := s.dispatch(ctx, candidate.entry, now, tick, kind)
				if admitted {
					candidate.dispatchedThisTick = true
					break
				}
				candidate.stopped = true
				if reason == ReasonInstanceMaxParallel {
					if !candidate.dispatchedThisTick {
						s.recordPoolSkip(candidate.entry)
					}
					break
				}
			}
		}
		if !attempted {
			break
		}
	}
	if s.afterTick != nil {
		s.afterTick(ctx)
	}
}

func (s *Scheduler) orderedGaggles(gaggles []string) []string {
	sort.Strings(gaggles)
	if len(gaggles) < 2 {
		return gaggles
	}

	s.mu.Lock()
	last, hasLast := s.lastDispatchedGaggle, s.hasDispatchedGaggle
	s.mu.Unlock()
	if !hasLast {
		return gaggles
	}

	start := sort.Search(len(gaggles), func(i int) bool {
		return gaggles[i] > last
	})
	if start == len(gaggles) {
		start = 0
	}
	ordered := make([]string, 0, len(gaggles))
	ordered = append(ordered, gaggles[start:]...)
	ordered = append(ordered, gaggles[:start]...)
	return ordered
}

func (s *Scheduler) recordGaggleDispatch(gaggle string) {
	s.mu.Lock()
	s.lastDispatchedGaggle = gaggle
	s.hasDispatchedGaggle = true
	s.mu.Unlock()
}

// Reload atomically replaces the configured workflows between scheduler ticks.
// Already-dispatched runs retain the WorkflowEntry (and Starter) captured by
// dispatch, while subsequent ticks and triggers resolve the replacement entry.
// The accepted change is journaled before it becomes active; a journal failure
// leaves the current configuration untouched.
func (s *Scheduler) Reload(entries []WorkflowEntry, openPRs OpenPRCounter, now time.Time, oldDigest, newDigest string) error {
	workflows := make(map[WorkflowIdentity]WorkflowEntry, len(entries))
	triggers := make(map[WorkflowIdentity]TriggerState, len(entries))
	backlogLastCheck := make(map[WorkflowIdentity]time.Time, len(entries))
	consecutivePoolSkips := make(map[WorkflowIdentity]int, len(entries))

	s.tickMu.Lock()
	defer s.tickMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range entries {
		identity := entryIdentity(entry)
		workflows[identity] = entry
		state, ok := s.triggers[identity]
		if !ok {
			state = TriggerState{Workflow: entry.Workflow, LastEval: now}
		}
		state.Workflow = entry.Workflow
		state.Schedules = entry.Schedules
		triggers[identity] = state
		if checked, ok := s.backlogLastCheck[identity]; ok {
			backlogLastCheck[identity] = checked
		}
		if skips := s.consecutivePoolSkips[identity]; skips > 0 {
			consecutivePoolSkips[identity] = skips
		}
	}

	if err := s.appendJournalEvent(journal.Event{
		Type: journal.EventConfigReloaded,
		Runner: map[string]any{
			"oldDigest": oldDigest,
			"newDigest": newDigest,
		},
	}); err != nil {
		return fmt.Errorf("localscheduler: journal config reload: %w", err)
	}
	evaluations := make(map[WorkflowIdentity]time.Time, len(triggers))
	for identity, state := range triggers {
		evaluations[identity] = state.LastEval
	}
	if err := s.persistTriggerEvaluationsLocked(evaluations); err != nil {
		return err
	}

	s.conditions.SetOpenPRCounter(openPRs)
	s.workflows = workflows
	s.triggers = triggers
	s.backlogLastCheck = backlogLastCheck
	s.consecutivePoolSkips = consecutivePoolSkips

	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// pollBacklog returns the current demand for a backlog-triggered workflow when
// its provider polling interval is due.
func (s *Scheduler) pollBacklog(ctx context.Context, entry WorkflowEntry, now time.Time) int {
	identity := entryIdentity(entry)
	s.mu.Lock()
	last := s.backlogLastCheck[identity]
	due := last.IsZero() || !now.Before(last.Add(backlogPollInterval))
	if due {
		s.backlogLastCheck[identity] = now
	}
	s.mu.Unlock()
	if !due {
		return 0
	}

	ready, err := entry.BacklogCounter.EligibleCount(ctx)
	if err != nil {
		s.journalEvent(journal.Event{
			Type:     journal.EventError,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Error:    &journal.ErrorDetail{Code: "backlog_count_failed", Message: err.Error()},
		})
		return 0
	}
	if ready < 0 {
		return 0
	}
	return ready
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
	var entry WorkflowEntry
	matches := 0
	for identity, candidate := range s.workflows {
		if identity.Workflow == workflow {
			entry = candidate
			matches++
		}
	}
	s.mu.Unlock()
	if matches == 0 {
		return "", fmt.Errorf("localscheduler: unknown workflow %q", workflow)
	}
	if matches > 1 {
		return "", fmt.Errorf("localscheduler: workflow %q is ambiguous across gaggles", workflow)
	}
	runID, admitted, skipReason := s.dispatch(ctx, entry, now, TickResult{Fire: true, LastEval: now}, journal.TriggerManual)
	if !admitted {
		return "", &TriggerRejectedError{Workflow: workflow, Reason: skipReason}
	}
	return runID, nil
}

// TriggerRejectedError reports a trigger that run conditions refused. It
// carries the stable Reason string so a caller can tell a refusal that a
// later attempt could satisfy (a capacity condition — some other run holds
// the slot right now) from one that a retry can never satisfy (a budget is
// spent, the open-PR cap is reached). The message is unchanged from the
// plain fmt.Errorf this replaced.
type TriggerRejectedError struct {
	Workflow string
	Reason   string
}

func (e *TriggerRejectedError) Error() string {
	return fmt.Sprintf("localscheduler: run conditions rejected the trigger for %q: %s", e.Workflow, e.Reason)
}

// Transient reports whether waiting could plausibly clear the refusal.
//
// Only the max-parallel conditions qualify: they are held by runs that are
// already in flight and release as those runs finish. This matters because a
// slot is released strictly *after* the run it belongs to is observable as
// terminal — dispatch's `defer ReleaseWorkflow` runs once Starter.Start
// returns, while a client watching the run's own journal (waitForRunTerminal,
// what `goobers run` does) sees the terminal event the runner wrote inside
// that call. So back-to-back `goobers run X` invocations can race the release
// of the slot the first one just finished with, and the second is refused for
// capacity that is about to exist. Budget/quota/open-PR-cap refusals are not
// transient in this sense and must still fail fast.
func (e *TriggerRejectedError) Transient() bool {
	return e.Reason == ReasonMaxParallel || e.Reason == ReasonInstanceMaxParallel
}

// RecordTriggerRefusal journals a trigger rejected by an admission layer
// before Scheduler.Trigger could safely dispatch it.
func (s *Scheduler) RecordTriggerRefusal(workflow, reason string) {
	s.journalEvent(journal.Event{
		Type:     journal.EventTickSkipped,
		Workflow: workflow,
		Reason:   reason,
	})
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
// run ids of every workflow actually admitted, in bounded-fair gaggle order
// and workflow-name order within each gaggle.
func (s *Scheduler) Signal(ctx context.Context, name string, now time.Time) []string {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()

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
	sort.Slice(subscribed, func(i, j int) bool {
		if subscribed[i].Gaggle != subscribed[j].Gaggle {
			return subscribed[i].Gaggle < subscribed[j].Gaggle
		}
		return subscribed[i].Workflow < subscribed[j].Workflow
	})

	byGaggle := make(map[string][]WorkflowEntry)
	gaggleNames := make([]string, 0)
	for _, entry := range subscribed {
		if _, ok := byGaggle[entry.Gaggle]; !ok {
			gaggleNames = append(gaggleNames, entry.Gaggle)
		}
		byGaggle[entry.Gaggle] = append(byGaggle[entry.Gaggle], entry)
	}
	gaggleNames = s.orderedGaggles(gaggleNames)

	var runIDs []string
	next := make(map[string]int, len(gaggleNames))
	for {
		attempted := false
		for _, gaggle := range gaggleNames {
			for next[gaggle] < len(byGaggle[gaggle]) {
				entry := byGaggle[gaggle][next[gaggle]]
				next[gaggle]++
				attempted = true
				runID, admitted, reason := s.dispatch(ctx, entry, now, TickResult{Fire: true, LastEval: now}, journal.TriggerSignal)
				if admitted {
					runIDs = append(runIDs, runID)
					break
				}
				if reason == ReasonInstanceMaxParallel {
					break
				}
			}
		}
		if !attempted {
			break
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
// The telemetry span it opens covers only the decision (trigger -> admit/skip),
// not the run itself: entry.Starter.Start runs in its own
// goroutine below and outlives dispatch's return, so the run gets its own
// root span (via runner.Runner.startRunSpan). The candidate run ID is minted
// first so both spans share its trace even when admission blocks the dispatch.
func (s *Scheduler) dispatch(ctx context.Context, entry WorkflowEntry, now time.Time, tick TickResult, kind journal.TriggerKind) (runID string, admitted bool, skipReason string) {
	runID, err := newRunID()
	if err != nil {
		reason := "run-id generation failed: " + err.Error()
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason:   reason,
		})
		return "", false, reason
	}

	span := s.startSpan(ctx, entry, runID)
	defer span.End()

	// Unlike the journalEvent calls below (best-effort: they record a
	// decision already made, so a write failure doesn't roll it back), a
	// failed trigger.fired append MUST stop this dispatch here rather than
	// being swallowed (SCH-031, issue #142): ReconstructLastEval rebuilds
	// each workflow's LastEval from scheduled trigger.fired history after a
	// restart, so a scheduled fire that started a run but never durably
	// recorded having fired would replay on the very next restart —
	// dispatching a second run for the same nominal firing. Refusing every
	// trigger kind keeps the invariant that a run only ever starts once its
	// trigger.fired record has durably landed.
	if err := s.appendJournalEvent(journal.Event{
		Type:     journal.EventTriggerFired,
		Workflow: entry.Workflow,
		Gaggle:   entry.Gaggle,
		Reason:   fireReason(tick, kind),
	}); err != nil {
		reason := "trigger.fired journal write failed: " + err.Error()
		span.Fail(err)
		return "", false, reason
	}

	identity := entryIdentity(entry)
	// Schedule-time runner-capability match (RRQ-1/#1101): refuse the run
	// before it can consume an admission slot when the runner does not claim a
	// capability the workflow's gaggle/stages require. This is the runtime,
	// per-run enforcement of the same invariant the config-load cross-check
	// (instance.CheckCapabilityRequirements) guards statically — the load-bearing
	// seam a future dynamic/multi-runner router grows from — so a missing claim
	// fails a run to schedule rather than scheduling it to fail at run.
	if missing := s.runnerCapabilities.Missing(entry.RequiredCapabilities); len(missing) > 0 {
		reason := ReasonMissingCapability + ": " + strings.Join(missing, ", ")
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason:   reason,
		})
		span.Complete(telemetry.OutcomeBlocked, false)
		return "", false, reason
	}
	ok, reason := s.conditions.AdmitWorkflow(identity, entry.Readiness, now)
	if !ok {
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason:   reason,
		})
		span.Complete(telemetry.OutcomeBlocked, false)
		return "", false, reason
	}
	s.resetPoolSkips(identity)
	s.recordGaggleDispatch(entry.Gaggle)

	s.journalEvent(journal.Event{
		Type:     journal.EventRunStarted,
		Workflow: entry.Workflow,
		Gaggle:   entry.Gaggle,
		RunID:    runID,
	})
	span.Succeed("started: " + runID)

	s.mu.Lock()
	s.admittedRuns[runID] = identity
	s.mu.Unlock()
	s.dispatches.Add(1)
	go func() {
		defer s.dispatches.Done()
		defer s.ReleaseRun(runID, entry.Workflow)
		entry.Starter = gooberDigestStarter{digest: entry.GooberDigest, next: entry.Starter}
		result, startErr := entry.Starter.Start(ctx, StartRequest{
			RunID:   runID,
			Gaggle:  entry.Gaggle,
			Trigger: journal.Trigger{Kind: kind, Ref: entry.Workflow},
			RepoRef: entry.RepoRef,
		})
		// #710: this echo used to carry only the bare phase string — a
		// business failure (result.Phase == "failed", startErr == nil: the
		// run completed dispatch cleanly and reported a failed OUTCOME)
		// journaled as a content-free status:"failed", even though the real
		// cause (a stage's own errorCode/message) was sitting one journal
		// line above in the run's own stage.finished event the entire time
		// (#705's root cause). result.FailureStage/Code/Message (threaded
		// from runner.Result through StartResult, starter.go's field-for-
		// field mirror) carry that cause here. The infra-error branch below
		// is deliberately untouched: a genuine Go dispatch error already
		// carries its own full detail in startErr, and FailureCode is not
		// populated in that path anyway.
		ev := journal.Event{
			Type:     journal.EventRunFinished,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			RunID:    runID,
			Status:   string(result.Phase),
		}
		switch {
		case startErr != nil:
			ev.Status = "error: " + startErr.Error()
		case result.FailureCode != "":
			ev.Stage = result.FailureStage
			ev.Error = &journal.ErrorDetail{Code: result.FailureCode, Message: result.FailureMessage}
			if result.FailureStage != "" {
				ev.Status = fmt.Sprintf("%s (%s: %s)", ev.Status, result.FailureStage, result.FailureCode)
			} else {
				ev.Status = fmt.Sprintf("%s (%s)", ev.Status, result.FailureCode)
			}
		}
		s.journalEvent(ev)
	}()
	return runID, true, ""
}

func (s *Scheduler) resetPoolSkips(identity WorkflowIdentity) {
	s.mu.Lock()
	delete(s.consecutivePoolSkips, identity)
	s.mu.Unlock()
}

func (s *Scheduler) recordPoolSkip(entry WorkflowEntry) {
	identity := entryIdentity(entry)
	s.mu.Lock()
	s.consecutivePoolSkips[identity]++
	skips := s.consecutivePoolSkips[identity]
	s.mu.Unlock()

	if skips == starvationSkipThreshold {
		s.journalEvent(journal.Event{
			Type:      journal.EventWorkflowStarved,
			Workflow:  entry.Workflow,
			Gaggle:    entry.Gaggle,
			Reason:    fmt.Sprintf("consecutive instance pool skips: %d", skips),
			SkipCount: skips,
		})
	}
}

// startSpan opens a scheduler decision span for entry's dispatch, if
// telemetry is configured. A zero telemetry.Span is safe to use (its methods
// no-op), so callers need no nil checks.
func (s *Scheduler) startSpan(ctx context.Context, entry WorkflowEntry, runID string) telemetry.Span {
	if s.telemetry == nil {
		return telemetry.Span{}
	}
	attrs := telemetry.SchedulerAttributes{
		Gaggle:          entry.Gaggle,
		WorkflowID:      entry.Workflow,
		WorkflowVersion: strconv.Itoa(entry.WorkflowVersion),
		WorkflowDigest:  entry.WorkflowDigest,
		GooberDigest:    entry.GooberDigest,
		RunID:           runID,
		Action:          "dispatch",
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
	case journal.TriggerItem:
		return "backlog item ready"
	}
	if tick.CatchUp {
		return fmt.Sprintf("%s(missed %d)", triggerReasonCatchUpPrefix, tick.MissedTicks)
	}
	return triggerReasonScheduled
}

// nextWakeup computes how long to sleep until the earliest workflow trigger is
// next due, so Run idles instead of busy-polling. A workflow with neither a
// schedule nor a BacklogCounter (manual-only) doesn't contribute; if none are
// cron- or backlog-managed, it returns a conservative default so the loop
// still wakes periodically for Reconcile-style housekeeping rather than
// blocking forever.
func (s *Scheduler) nextWakeup(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	var earliest time.Time
	consider := func(next time.Time) {
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
		}
	}
	for name, entry := range s.workflows {
		ts := s.triggers[name]
		if next, ok := NextScheduledFire(entry.Schedules, ts.LastEval); ok {
			consider(next)
		}
		if entry.BacklogCounter != nil {
			// pollBacklog's own due check, mirrored here so the Run loop
			// wakes in time to poll it (#344) — otherwise a mixed instance
			// with both schedule- and backlog-item-triggered workflows
			// could starve the latter's poll cadence down to whatever the
			// LONGEST schedule gap happens to be.
			consider(s.backlogLastCheck[name].Add(backlogPollInterval))
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

func (s *Scheduler) triggerEvaluationsLocked() map[WorkflowIdentity]time.Time {
	evaluations := make(map[WorkflowIdentity]time.Time, len(s.triggers))
	for identity, state := range s.triggers {
		evaluations[identity] = state.LastEval
	}
	return evaluations
}

func (s *Scheduler) persistTriggerEvaluationsLocked(evaluations map[WorkflowIdentity]time.Time) error {
	if s.log == nil {
		return nil
	}
	return s.writeTriggerState(s.log.Dir(), evaluations)
}

func scheduledTriggerFired(reason string) bool {
	return reason == "" || reason == triggerReasonScheduled || strings.HasPrefix(reason, triggerReasonCatchUpPrefix)
}
