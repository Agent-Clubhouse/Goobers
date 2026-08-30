package localscheduler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/telemetry"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
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
	// ScheduleBackoffs aligns with Schedules. Missing entries use the default
	// adaptive idle policy.
	ScheduleBackoffs []IdleBackoffConfig
	Signals          []string
	// PollFallbackCause, when non-empty, explains why a scheduled fire is the
	// fallback for an event-capable workflow.
	PollFallbackCause string
	// BacklogCounter, when set, marks this workflow as backlog-item-triggered
	// (#344): Tick polls it every backlogPollInterval instead of (or in
	// addition to, if Schedules is also set) evaluating a cron schedule, and
	// fans out up to that many runs at once — unlike a schedule trigger's
	// fixed one-shot-per-fire model, a backlog-item trigger starts as many
	// runs as there are ready items, bounded by run conditions.
	BacklogCounter BacklogCounter
	// PollProvider identifies the quota ledger used by BacklogCounter.
	PollProvider apiv1.Provider
	// PollPriority preserves higher-priority polls when a provider budget cannot
	// cover every poll due in one tick.
	PollPriority int32
	// ScheduleDemandCounter sizes a due schedule fire to the eligible work
	// available at that instant. Nil preserves the ordinary one-run-per-fire
	// schedule behavior.
	ScheduleDemandCounter BacklogCounter
	// RefillDemandCounter reports whether queue work is currently eligible for
	// desired-concurrency refill. It is polled on a bounded cadence and never
	// turns an ordinary trigger into backlog fan-out.
	RefillDemandCounter BacklogCounter
	Starter             Starter
	RepoRef             apiv1.RepoRef
	// RequiredCapabilities is the union of runner (toolchain/platform)
	// capabilities this workflow's gaggle and stages require (RRQ-1/#1101).
	// dispatch refuses the run before admission when the runner does not claim
	// every entry. Nil/empty imposes no requirement.
	RequiredCapabilities []string
	// PlacementRefusal, when non-empty, marks this workflow refused by the
	// startup constraint solve against the instance's declared runners:
	// inventory (dsl-3.0.md §5 checkpoint 3, #2860's boot-never-kills
	// ruling): the daemon starts and every other workflow serves, while this
	// workflow's runs are refused with this named diagnostic — journaled as
	// workflow.refused when the scheduler learns the entry, and again as
	// tick.skipped on any dispatch attempt. Set only when a runners:
	// inventory is declared; zero-declaration instances never set it, so
	// their per-run capability check below stays their only refusal path,
	// byte-identical to previous releases.
	PlacementRefusal string
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

// ProviderQuotaGuardedBacklogCounter can consult a local snapshot before its
// request gate spends provider quota.
type ProviderQuotaGuardedBacklogCounter interface {
	BacklogCounter
	ProviderQuotaGuarded() bool
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

const (
	defaultIdleBackoffFloor   = time.Minute
	defaultIdleBackoffCeiling = 15 * time.Minute
	refillTriggerReason       = "refill occupancy"
	refillBlockedReasonPrefix = "refill blocked: "
)

// IdleBackoffConfig is the runtime form of a schedule trigger's idle policy.
type IdleBackoffConfig struct {
	Enabled bool
	Floor   time.Duration
	Ceiling time.Duration
}

// ParseIdleBackoff applies schedule-trigger defaults and validates duration
// ordering for runtime wiring.
func ParseIdleBackoff(backoff *apiv1.IdleBackoff) (IdleBackoffConfig, error) {
	config := IdleBackoffConfig{
		Enabled: true,
		Floor:   defaultIdleBackoffFloor,
		Ceiling: defaultIdleBackoffCeiling,
	}
	if backoff == nil {
		return config, nil
	}
	if backoff.Enabled != nil {
		config.Enabled = *backoff.Enabled
	}
	var err error
	if backoff.Floor != "" {
		config.Floor, err = time.ParseDuration(backoff.Floor)
		if err != nil || config.Floor <= 0 {
			return IdleBackoffConfig{}, fmt.Errorf("localscheduler: idleBackoff floor %q must be a positive duration", backoff.Floor)
		}
	}
	if backoff.Ceiling != "" {
		config.Ceiling, err = time.ParseDuration(backoff.Ceiling)
		if err != nil || config.Ceiling <= 0 {
			return IdleBackoffConfig{}, fmt.Errorf("localscheduler: idleBackoff ceiling %q must be a positive duration", backoff.Ceiling)
		}
	}
	if config.Ceiling < config.Floor {
		return IdleBackoffConfig{}, fmt.Errorf("localscheduler: idleBackoff ceiling %s must not be below floor %s", config.Ceiling, config.Floor)
	}
	return config, nil
}

// demandPollTimeout bounds provider-backed demand checks while Tick holds
// tickMu so signals and configuration reloads cannot stall indefinitely.
const demandPollTimeout = 45 * time.Second

const starvationSkipThreshold = 3

// newRunID is the run-id generator; swappable in tests for determinism.
var newRunID = telemetry.NewRunID

// SpanStarter is the slice of the telemetry client the local scheduler needs
// to open a decision span per dispatch (issue #126). *telemetry.Client
// satisfies it structurally — the narrow-interface pattern keeps the
// scheduler off telemetry's full surface.
type SpanStarter interface {
	StartSchedulerSpan(ctx context.Context, attrs telemetry.SchedulerAttributes) (context.Context, telemetry.Span, error)
}

type runAdmission struct {
	identity   WorkflowIdentity
	generation uint64
	owners     int
	retained   bool
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
	providerQuota     ProviderQuotaGate
	demandPollTimeout time.Duration
	afterTick         func(context.Context)
	heartbeatInterval time.Duration
	refreshHeartbeat  func(time.Time) error
	// onPollProgress, if set, is called after every individual
	// provider-backed demand poll Tick issues while holding tickMu (#3806) —
	// not just once when Tick itself returns. A tick with several due
	// polls, each bounded only by demandPollTimeout (45s), can otherwise
	// leave refreshHeartbeat's once-per-Run staleness window open for
	// several minutes: long enough for a liveness probe reading that
	// heartbeat to kill a busy-but-healthy daemon mid-tick. Unlike
	// refreshHeartbeat, this callback MUST be cheap and non-blocking — it
	// is called from inside the tickMu-held critical section once per poll,
	// so it must never itself touch disk or a provider.
	onPollProgress    func(time.Time)
	writeTriggerState func(string, map[WorkflowIdentity]time.Time) error
	// stateOwner is the M5 generation/ownership guard for the shared state
	// files this scheduler rewrites (stateguard.go): a second daemon against
	// the same instance root trips ErrStateSeized instead of a data race.
	stateOwner *stateOwner

	mu          sync.Mutex
	admissionMu sync.Mutex
	tickMu      sync.Mutex
	triggers    map[WorkflowIdentity]TriggerState
	dispatches  sync.WaitGroup
	wake        chan struct{}
	// reconciledRuns identifies the pre-existing runs represented in
	// Conditions' startup counts, so recovery releases cannot consume another
	// run's workflow-level slot.
	reconciledRuns map[string]WorkflowIdentity
	// admittedRuns identifies live dispatches and continuations whose shared
	// condition slot remains reserved until every owner releases it or a
	// watchdog terminalizes the run.
	admittedRuns            map[string]runAdmission
	nextAdmissionGeneration uint64
	// backlogLastCheck tracks, per backlog-item-triggered workflow, when its
	// BacklogCounter was last polled (#344) — separate from triggers'
	// LastEval, which is cron-Schedule-specific bookkeeping a workflow with
	// both trigger kinds must not have corrupted by backlog-check timing.
	// Reset to empty on every restart (not reconciled from durable history):
	// the worst case is one extra poll right after a restart, not a
	// correctness bug, so it isn't worth the added Reconcile complexity.
	backlogLastCheck map[WorkflowIdentity]time.Time
	refillLastCheck  map[WorkflowIdentity]time.Time
	idleBackoffs     map[WorkflowIdentity][]idleBackoffState
	// pendingScheduleDemand retains demand that a due scheduled poll found but
	// concurrency limits could not admit yet. A durable outstanding marker lets
	// Reconcile repoll current demand without replaying fully consumed firings or
	// persisting a count that may become stale while the daemon is down.
	pendingScheduleDemand map[WorkflowIdentity]scheduledDemand
	// consecutivePoolSkips ages workflows that were due and otherwise ready
	// but could not enter the shared instance concurrency pool.
	consecutivePoolSkips map[WorkflowIdentity]int
	// quotaResumePacing drains provider-backed workflow polls one workflow per
	// tick after an exhausted quota window resets.
	quotaResumePacing map[apiv1.Provider]bool
	// authCircuits suppress provider polls and dispatch for a workflow after a
	// permanent credential failure. Reload clears the circuit after an
	// operator repairs configuration.
	authCircuits map[WorkflowIdentity]struct{}
	// lastDispatchedGaggle is the cursor for work-conserving round-robin
	// dispatch across gaggles. hasDispatchedGaggle distinguishes the initial
	// state from a legacy single-gaggle entry whose gaggle name is empty.
	lastDispatchedGaggle string
	hasDispatchedGaggle  bool
	// selfRunner is the shared constraint solver's view of the local runner
	// (RRQ-1/#1101, rehomed onto internal/runnersolve by #3506 so the per-run
	// admit and the apply/boot solves are one implementation — dsl-3.0.md §5,
	// open point 8). Set once at construction, read-only thereafter, so it
	// needs no lock. A dispatch is refused before admission when the entry
	// requires a capability this runner does not satisfy. The zero value
	// claims nothing, which only matters for entries that declare
	// RequiredCapabilities — an entry that declares none is never refused on
	// this axis.
	//
	// CHECKPOINT 2 SEAM (#3513): this is the self-runner half of dispatch
	// admission only. The Temporal dispatch-time half — deriving the task
	// queue from the solver's eligible runner set and the bounded
	// Linux-preferring schedule-to-start wait of dsl-3.0.md D3 — is #3513's,
	// and consumes runnersolve.Solve placements, not this field.
	selfRunner          runnersolve.Runner
	targetedPRValidator func(context.Context, WorkflowEntry, int) error
	// refillBlockedUntil tracks the earliest time each workflow may attempt the
	// next refill after an admission rejection.
	refillBlockedUntil map[WorkflowIdentity]time.Time
	// refillBackoff is the minimum time between refill attempts for the same
	// workflow when the previous attempt was rejected.
	refillBackoff time.Duration
	// refillBackoffJitter randomizes retry windows to avoid synchronized retries.
	refillBackoffJitter time.Duration
	// refillRandN bounds testability of jitter generation.
	refillRandN func(int64) int64
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

// WithPollHeartbeat registers a callback fired after every individual
// provider-backed demand poll Tick issues, in addition to (not instead of)
// WithTickHeartbeat's once-per-Run refresh (#3806). mark must be cheap and
// non-blocking: it runs inside Tick's tickMu-held critical section, once per
// due poll, specifically so an in-memory liveness signal can stay fresh
// across a tick with several slow (up to demandPollTimeout) polls — a
// once-per-Run refresh alone cannot bound that window. nil (the default)
// installs no callback.
func WithPollHeartbeat(mark func(time.Time)) Option {
	return func(s *Scheduler) {
		s.onPollProgress = mark
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

// WithProviderQuota wires the shared provider-quota circuit breaker and
// polling budget. Optional — nil/unset leaves both unenforced.
func WithProviderQuota(gate ProviderQuotaGate) Option {
	return func(s *Scheduler) {
		if gate != nil {
			s.conditions.SetProviderQuota(gate)
			s.providerQuota = gate
		}
	}
}

// WithMemoryGate wires the cgroup-aware admission gate (#3949): when the
// daemon's own memory cgroup is near its limit, new runs are refused with
// ReasonMemoryPressure until it recovers, rather than being admitted into a
// container that is about to be OOM-killed. Optional — nil/unset leaves it
// unenforced, which is also what a gate reads as outside a container.
func WithMemoryGate(gate MemoryGate) Option {
	return func(s *Scheduler) {
		if gate != nil {
			s.conditions.SetMemoryGate(gate)
		}
	}
}

// WithRunnerCapabilities declares the local runner's static advertised
// capability set (RRQ-1/#1101). A dispatch whose entry requires a capability
// not in this set is refused before admission, journaling a tick.skipped with a
// ReasonMissingCapability diagnostic naming the gap. Optional — unset claims
// nothing, so only entries that declare RequiredCapabilities are ever affected.
// Internally the claims become the shared solver's self-runner view
// (runnersolve.SelfRunner), whose match is byte-identical to the former
// runnercap union check for every declared token.
func WithRunnerCapabilities(caps []string) Option {
	return func(s *Scheduler) {
		s.selfRunner = runnersolve.SelfRunner(caps)
	}
}

// WithTargetedPRValidator validates a manually targeted pull request before a
// signal trigger is admitted. It is optional so the scheduler package remains
// independent of provider construction.
func WithTargetedPRValidator(validate func(context.Context, WorkflowEntry, int) error) Option {
	return func(s *Scheduler) {
		s.targetedPRValidator = validate
	}
}

// New builds a Scheduler over the given workflow entries. Call Reconcile
// before Run to seed run-condition and trigger state from durable state after
// a restart; a freshly-created instance can skip it (everything starts empty).
func New(entries []WorkflowEntry, log *journal.InstanceLog, opts ...Option) *Scheduler {
	s := &Scheduler{
		workflows:             make(map[WorkflowIdentity]WorkflowEntry, len(entries)),
		conditions:            NewConditions(),
		log:                   log,
		now:                   time.Now,
		after:                 time.After,
		demandPollTimeout:     demandPollTimeout,
		triggers:              make(map[WorkflowIdentity]TriggerState),
		reconciledRuns:        make(map[string]WorkflowIdentity),
		admittedRuns:          make(map[string]runAdmission),
		backlogLastCheck:      make(map[WorkflowIdentity]time.Time),
		refillLastCheck:       make(map[WorkflowIdentity]time.Time),
		idleBackoffs:          make(map[WorkflowIdentity][]idleBackoffState),
		pendingScheduleDemand: make(map[WorkflowIdentity]scheduledDemand),
		consecutivePoolSkips:  make(map[WorkflowIdentity]int),
		quotaResumePacing:     make(map[apiv1.Provider]bool),
		authCircuits:          make(map[WorkflowIdentity]struct{}),
		refillBlockedUntil:    make(map[WorkflowIdentity]time.Time),
		refillBackoff:         30 * time.Second,
		refillBackoffJitter:   5 * time.Second,
		refillRandN:           rand.Int64N,
		wake:                  make(chan struct{}, 1),
		stateOwner:            newStateOwner(),
	}
	s.writeTriggerState = func(schedulerDir string, evaluations map[WorkflowIdentity]time.Time) error {
		return writeTriggerEvaluations(schedulerDir, s.stateOwner, evaluations)
	}
	for _, opt := range opts {
		opt(s)
	}
	for _, e := range entries {
		identity := entryIdentity(e)
		s.workflows[identity] = e
		ts := TriggerState{Workflow: e.Workflow, Schedules: e.Schedules, LastEval: s.now()}
		s.triggers[identity] = ts
		s.idleBackoffs[identity] = make([]idleBackoffState, len(e.Schedules))
	}
	s.journalPlacementRefusals(entries)
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

	outstandingScheduleDemand, err := readScheduleDemandState(s.log.Dir())
	if err != nil {
		return err
	}
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
		if outstandingScheduleDemand[identity] {
			pending := scheduledDemand{
				schedule:  TickResult{Fire: true, LastEval: ts.LastEval},
				remaining: 1,
			}
			if s.workflows[identity].ScheduleDemandCounter != nil {
				pending = scheduledDemand{repoll: true}
			}
			s.pendingScheduleDemand[identity] = pending
		}
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
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	s.mu.Lock()
	reconciledWorkflow, ok := s.reconciledRuns[runID]
	if ok && reconciledWorkflow.Workflow == workflow {
		delete(s.reconciledRuns, runID)
	}
	s.mu.Unlock()
	if ok && reconciledWorkflow.Workflow == workflow {
		s.conditions.ReleaseWorkflow(reconciledWorkflow)
		s.wakeForDemand(reconciledWorkflow)
	}
}

// ReleaseRun force-releases a run's live admission or restart-reconciled slot
// exactly once. Watchdogs use this after terminalizing a run; generation-scoped
// dispatch and continuation cleanup may safely run afterward.
func (s *Scheduler) ReleaseRun(runID, workflow string) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	s.mu.Lock()
	admission, admitted := s.admittedRuns[runID]
	if admitted && admission.identity.Workflow == workflow {
		delete(s.admittedRuns, runID)
	}
	reconciledIdentity, reconciled := s.reconciledRuns[runID]
	if reconciled && reconciledIdentity.Workflow == workflow {
		delete(s.reconciledRuns, runID)
	}
	s.mu.Unlock()

	released := false
	var releasedIdentity WorkflowIdentity
	switch {
	case admitted && admission.identity.Workflow == workflow:
		s.conditions.ReleaseWorkflow(admission.identity)
		released = true
		releasedIdentity = admission.identity
	case reconciled && reconciledIdentity.Workflow == workflow:
		s.conditions.ReleaseWorkflow(reconciledIdentity)
		released = true
		releasedIdentity = reconciledIdentity
	}
	if released {
		s.wakeForDemand(releasedIdentity)
	}
}

func (s *Scheduler) releaseAdmissionOwner(runID, workflow string, generation uint64) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()

	s.mu.Lock()
	admission, admitted := s.admittedRuns[runID]
	if !admitted || admission.identity.Workflow != workflow || admission.generation != generation {
		s.mu.Unlock()
		return
	}
	admission.owners--
	if admission.owners > 0 || admission.retained {
		s.admittedRuns[runID] = admission
		s.mu.Unlock()
		return
	}
	delete(s.admittedRuns, runID)
	s.mu.Unlock()

	s.conditions.ReleaseWorkflow(admission.identity)
	s.wakeForDemand(admission.identity)
}

func (s *Scheduler) admissionOwnerRelease(runID, workflow string, generation uint64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.releaseAdmissionOwner(runID, workflow, generation)
		})
	}
}

// RetainContinuation keeps a run's condition slot reserved between
// interventions without accumulating one owner per pause.
func (s *Scheduler) RetainContinuation(runID, workflow string) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	admission, admitted := s.admittedRuns[runID]
	if !admitted || admission.identity.Workflow != workflow {
		return
	}
	admission.retained = true
	s.admittedRuns[runID] = admission
}

// ReleaseRetainedContinuation drops the between-interventions hold while
// preserving any dispatch or active-continuation owners.
func (s *Scheduler) ReleaseRetainedContinuation(runID, workflow string) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()

	s.mu.Lock()
	admission, admitted := s.admittedRuns[runID]
	if !admitted || admission.identity.Workflow != workflow || !admission.retained {
		s.mu.Unlock()
		return
	}
	admission.retained = false
	if admission.owners > 0 {
		s.admittedRuns[runID] = admission
		s.mu.Unlock()
		return
	}
	delete(s.admittedRuns, runID)
	s.mu.Unlock()

	s.conditions.ReleaseWorkflow(admission.identity)
	s.wakeForDemand(admission.identity)
}

// ReserveContinuation reserves the configured workflow's concurrency slot for
// an existing run without recording a second run start or consuming its rate
// budget. The returned release is idempotent.
func (s *Scheduler) ReserveContinuation(runID, gaggle, workflow string) (release func(), ok bool, reason string) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()

	identity := WorkflowIdentity{Gaggle: gaggle, Workflow: workflow}
	s.mu.Lock()
	entry, configured := s.workflows[identity]
	admission, admitted := s.admittedRuns[runID]
	_, reconciled := s.reconciledRuns[runID]
	switch {
	case !configured:
		s.mu.Unlock()
		return func() {}, false, "workflow unavailable"
	case admitted && admission.identity == identity:
		admission.owners++
		s.admittedRuns[runID] = admission
		s.mu.Unlock()
		return s.admissionOwnerRelease(runID, workflow, admission.generation), true, ""
	case admitted || reconciled:
		s.mu.Unlock()
		return func() {}, false, "run already admitted"
	}
	s.mu.Unlock()

	if ok, reason := s.conditions.ReserveContinuation(identity, entry.Readiness); !ok {
		return func() {}, false, reason
	}
	s.mu.Lock()
	s.nextAdmissionGeneration++
	generation := s.nextAdmissionGeneration
	s.admittedRuns[runID] = runAdmission{
		identity:   identity,
		generation: generation,
		owners:     1,
	}
	s.mu.Unlock()

	return s.admissionOwnerRelease(runID, workflow, generation), true, ""
}

func (s *Scheduler) wakeForDemand(identity WorkflowIdentity) {
	s.mu.Lock()
	pending := len(s.pendingScheduleDemand) > 0
	refill := false
	if entry, ok := s.workflows[identity]; ok &&
		entry.Readiness.DesiredConcurrentRuns > 0 &&
		entry.RefillDemandCounter != nil {
		delete(s.refillLastCheck, identity)
		refill = true
	}
	pacing := len(s.quotaResumePacing) > 0
	s.mu.Unlock()
	if (!pending && !refill) || pacing {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
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
	scheduleRemaining  int
	scheduleDemand     bool
	schedulePollDue    bool
	backlogPollDue     bool
	backlogRemaining   int
	refillRemaining    int
	refillPollDue      bool
	refillEligible     int
	poolSkips          int
	dispatchedThisTick bool
	stopped            bool
	scheduleIndexes    []int
}

type triggerSource uint8

const (
	triggerSourceSchedule triggerSource = iota + 1
	triggerSourceBacklog
	triggerSourceRefill
)

func (c *tickCandidate) next() (TickResult, journal.TriggerKind, triggerSource, bool) {
	if c.scheduleRemaining > 0 {
		c.scheduleRemaining--
		return c.schedule, journal.TriggerSchedule, triggerSourceSchedule, true
	}
	if c.backlogRemaining > 0 {
		c.backlogRemaining--
		return TickResult{Fire: true, LastEval: c.schedule.LastEval}, journal.TriggerItem, triggerSourceBacklog, true
	}
	if c.refillRemaining > 0 {
		c.refillRemaining--
		return TickResult{Fire: true, LastEval: c.schedule.LastEval}, journal.TriggerItem, triggerSourceRefill, true
	}
	return TickResult{}, "", 0, false
}

type tickGaggle struct {
	candidates []*tickCandidate
	nextIndex  int
}

func (g *tickGaggle) next() (*tickCandidate, TickResult, journal.TriggerKind, triggerSource, bool) {
	for range len(g.candidates) {
		candidate := g.candidates[g.nextIndex]
		g.nextIndex = (g.nextIndex + 1) % len(g.candidates)
		if candidate.stopped {
			continue
		}
		tick, kind, source, ok := candidate.next()
		if ok {
			return candidate, tick, kind, source, true
		}
	}
	return nil, TickResult{}, "", 0, false
}

// Tick evaluates every workflow's trigger at now, budgets provider-backed
// polls by priority, orders ready workflows by starvation age within each
// gaggle, and dispatches one item per ready gaggle per pass until demand or
// capacity is exhausted. The gaggle order resumes after the most recently
// admitted gaggle. With G continuously ready gaggles, this bounds a gaggle's
// wait to G-1 successful dispatches by other gaggles. Gaggles without ready
// work are omitted, so they never reserve capacity.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	ctx = providersnapshot.WithTick(ctx, now)

	s.mu.Lock()
	entries := make([]WorkflowEntry, 0, len(s.workflows))
	for _, e := range s.workflows {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].PollPriority != entries[j].PollPriority {
			return entries[i].PollPriority > entries[j].PollPriority
		}
		if entries[i].Workflow != entries[j].Workflow {
			return entries[i].Workflow < entries[j].Workflow
		}
		return entries[i].Gaggle < entries[j].Gaggle
	})
	providers := make(map[apiv1.Provider]struct{})
	for _, entry := range entries {
		providers[quotaProvider(entry.RepoRef.Provider)] = struct{}{}
		if entry.BacklogCounter != nil || entry.ScheduleDemandCounter != nil || entry.RefillDemandCounter != nil {
			providers[quotaProvider(entry.PollProvider)] = struct{}{}
		}
	}
	for provider := range providers {
		s.journalProviderQuotaReset(provider, now)
	}

	allCandidates := make([]*tickCandidate, 0, len(entries))
	for _, entry := range entries {
		if s.authCircuitOpen(entryIdentity(entry)) {
			continue
		}
		if entry.PlacementRefusal != "" {
			// Refused by the startup constraint solve (#2860, checkpoint 3):
			// journaled as workflow.refused when the entry was learned, and
			// refused with the named diagnostic on any explicit Trigger.
			// Skipped silently here (the auth-circuit idiom) so a permanently
			// refused workflow neither spends provider polls nor floods the
			// journal with a tick.skipped every tick.
			continue
		}
		identity := entryIdentity(entry)
		s.mu.Lock()
		pending := s.pendingScheduleDemand[identity]
		s.mu.Unlock()
		candidate := &tickCandidate{
			entry:             entry,
			schedule:          pending.schedule,
			scheduleRemaining: pending.remaining,
			scheduleDemand:    pending.remaining > 0,
			schedulePollDue:   pending.repoll,
		}
		if pending.remaining == 0 {
			candidate.schedule = TickResult{LastEval: now}
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
			dueIndexes := dueScheduleIndexes(entry.Schedules, ts.LastEval, now)
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
				candidate.scheduleIndexes = dueIndexes
				if blocked, reason := s.scheduleBackedOff(identity, entry, dueIndexes, now); blocked {
					s.journalEvent(journal.Event{
						Type:     journal.EventTickSkipped,
						Workflow: entry.Workflow,
						Gaggle:   entry.Gaggle,
						Reason:   reason,
					})
					candidate.scheduleIndexes = nil
				} else if entry.ScheduleDemandCounter == nil {
					candidate.scheduleRemaining = 1
					candidate.scheduleDemand = false
				} else {
					candidate.schedulePollDue = true
				}
			}
		}

		if entry.BacklogCounter != nil {
			candidate.backlogPollDue = s.backlogPollDue(entry, now)
		}
		if entry.RefillDemandCounter != nil &&
			entry.Readiness.DesiredConcurrentRuns > 0 &&
			s.conditions.ActiveWorkflow(identity) < int(entry.Readiness.DesiredConcurrentRuns) {
			s.mu.Lock()
			retryAfter := s.refillBlockedUntil[identity]
			lastCheck := s.refillLastCheck[identity]
			s.mu.Unlock()
			if !now.Before(retryAfter) &&
				(lastCheck.IsZero() || !now.Before(lastCheck.Add(backlogPollInterval))) {
				candidate.refillPollDue = true
			}
		}
		allCandidates = append(allCandidates, candidate)
	}

	s.pollDemandCounters(ctx, allCandidates, now)
	s.evaluateRefillOpportunities(allCandidates, now)
	s.paceQuotaResumedCandidates(allCandidates)
	candidates := make([]*tickCandidate, 0, len(allCandidates))
	for _, candidate := range allCandidates {
		if candidate.scheduleRemaining > 0 || candidate.backlogRemaining > 0 || candidate.refillRemaining > 0 {
			identity := entryIdentity(candidate.entry)
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
				candidate, tick, kind, source, ok := gaggle.next()
				if !ok {
					break
				}
				attempted = true
				trigger := journal.Trigger{Kind: kind, Ref: candidate.entry.Workflow}
				fire := fireReason(tick, kind)
				if source == triggerSourceRefill {
					fire = refillTriggerReason
				}
				if kind == journal.TriggerSchedule && candidate.entry.PollFallbackCause != "" {
					fire = "polling fallback: " + candidate.entry.PollFallbackCause
					if tick.CatchUp {
						fire += "; " + fireReason(tick, kind)
					}
				}
				var scheduleIndexes []int
				if kind == journal.TriggerSchedule {
					scheduleIndexes = candidate.scheduleIndexes
				}
				_, admitted, reason := s.dispatch(ctx, candidate.entry, now, trigger, fire, scheduleIndexes)
				if admitted {
					if kind == journal.TriggerSchedule && candidate.scheduleDemand {
						s.consumePendingScheduleDemand(candidate.entry)
					}
					candidate.dispatchedThisTick = true
					break
				}
				if kind == journal.TriggerSchedule && candidate.scheduleDemand &&
					reason != ReasonMaxParallel && reason != ReasonInstanceMaxParallel {
					s.clearPendingScheduleDemand(candidate.entry)
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
	idleBackoffs := make(map[WorkflowIdentity][]idleBackoffState, len(entries))
	pendingScheduleDemand := make(map[WorkflowIdentity]scheduledDemand, len(entries))
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
		idleBackoffs[identity] = make([]idleBackoffState, len(entry.Schedules))
		if pending, ok := s.pendingScheduleDemand[identity]; ok {
			pendingScheduleDemand[identity] = pending
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
	// Re-record refusals for the accepted configuration: the config.reloaded
	// event above marks the boundary, so a status reader always sees the
	// refusals current for the configuration now in force (#2860).
	s.journalPlacementRefusals(entries)
	evaluations := make(map[WorkflowIdentity]time.Time, len(triggers))
	for identity, state := range triggers {
		evaluations[identity] = state.LastEval
	}
	if err := s.persistTriggerEvaluationsLocked(evaluations); err != nil {
		return err
	}
	outstandingScheduleDemand := make(map[WorkflowIdentity]bool, len(pendingScheduleDemand))
	for identity, pending := range pendingScheduleDemand {
		if pending.remaining > 0 || pending.repoll {
			outstandingScheduleDemand[identity] = true
		}
	}
	if err := writeScheduleDemandState(s.log.Dir(), s.stateOwner, outstandingScheduleDemand); err != nil {
		return err
	}

	s.conditions.SetOpenPRCounter(openPRs)
	s.workflows = workflows
	s.triggers = triggers
	s.backlogLastCheck = backlogLastCheck
	s.idleBackoffs = idleBackoffs
	s.pendingScheduleDemand = pendingScheduleDemand
	s.consecutivePoolSkips = consecutivePoolSkips
	s.authCircuits = make(map[WorkflowIdentity]struct{})

	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *Scheduler) backlogPollDue(entry WorkflowEntry, now time.Time) bool {
	identity := entryIdentity(entry)
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.backlogLastCheck[identity]
	return last.IsZero() || !now.Before(last.Add(backlogPollInterval))
}

func (s *Scheduler) recordBacklogPoll(entry WorkflowEntry, now time.Time) {
	s.mu.Lock()
	s.backlogLastCheck[entryIdentity(entry)] = now
	s.mu.Unlock()
}

func (s *Scheduler) recordRefillPoll(entry WorkflowEntry, now time.Time) {
	s.mu.Lock()
	s.refillLastCheck[entryIdentity(entry)] = now
	s.mu.Unlock()
}

type demandPoll struct {
	candidate *tickCandidate
	counter   BacklogCounter
	schedule  bool
	refill    bool
}

type scheduledDemand struct {
	schedule  TickResult
	remaining int
	repoll    bool
}

type idleBackoffState struct {
	consecutive int
	interval    time.Duration
	nextPoll    time.Time
	generation  uint64
}

type idleBackoffToken struct {
	index      int
	generation uint64
}

func dueScheduleIndexes(schedules []Schedule, lastEval, now time.Time) []int {
	var due []int
	for index, schedule := range schedules {
		next := schedule.Next(lastEval)
		if !next.IsZero() && !next.After(now) {
			due = append(due, index)
		}
	}
	return due
}

func scheduleBackoffConfig(entry WorkflowEntry, index int) IdleBackoffConfig {
	config := IdleBackoffConfig{
		Enabled: true,
		Floor:   defaultIdleBackoffFloor,
		Ceiling: defaultIdleBackoffCeiling,
	}
	if index >= len(entry.ScheduleBackoffs) {
		return config
	}
	config = entry.ScheduleBackoffs[index]
	if config.Floor <= 0 {
		config.Floor = defaultIdleBackoffFloor
	}
	if config.Ceiling <= 0 {
		config.Ceiling = defaultIdleBackoffCeiling
	}
	return config
}

func (s *Scheduler) scheduleBackedOff(identity WorkflowIdentity, entry WorkflowEntry, indexes []int, now time.Time) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	states := s.idleBackoffs[identity]
	var next time.Time
	consecutive := 0
	for _, index := range indexes {
		config := scheduleBackoffConfig(entry, index)
		if !config.Enabled || index >= len(states) || !states[index].nextPoll.After(now) {
			return false, ""
		}
		if next.IsZero() || states[index].nextPoll.Before(next) {
			next = states[index].nextPoll
		}
		if states[index].consecutive > consecutive {
			consecutive = states[index].consecutive
		}
	}
	if next.IsZero() {
		return false, ""
	}
	return true, fmt.Sprintf("idle backoff: next poll at %s after %d consecutive no-work run(s)", next.UTC().Format(time.RFC3339), consecutive)
}

func (s *Scheduler) beginScheduledPoll(identity WorkflowIdentity, indexes []int) []idleBackoffToken {
	if len(indexes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	states := s.idleBackoffs[identity]
	tokens := make([]idleBackoffToken, 0, len(indexes))
	for _, index := range indexes {
		if index >= len(states) {
			continue
		}
		states[index].generation++
		tokens = append(tokens, idleBackoffToken{index: index, generation: states[index].generation})
	}
	s.idleBackoffs[identity] = states
	return tokens
}

func (s *Scheduler) recordScheduledPollResult(identity WorkflowIdentity, entry WorkflowEntry, tokens []idleBackoffToken, noWork bool, completedAt time.Time) {
	if len(tokens) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	states := s.idleBackoffs[identity]
	for _, token := range tokens {
		if token.index >= len(states) || states[token.index].generation != token.generation {
			continue
		}
		config := scheduleBackoffConfig(entry, token.index)
		if !config.Enabled || !noWork {
			states[token.index].consecutive = 0
			states[token.index].interval = 0
			states[token.index].nextPoll = time.Time{}
			continue
		}
		states[token.index].consecutive++
		if states[token.index].interval < config.Floor {
			states[token.index].interval = config.Floor
		} else if states[token.index].interval >= config.Ceiling/2 {
			states[token.index].interval = config.Ceiling
		} else {
			states[token.index].interval *= 2
		}
		if states[token.index].interval > config.Ceiling {
			states[token.index].interval = config.Ceiling
		}
		states[token.index].nextPoll = completedAt.Add(states[token.index].interval)
	}
	s.idleBackoffs[identity] = states
}

func (s *Scheduler) resetIdleBackoff(identity WorkflowIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()

	states := s.idleBackoffs[identity]
	for index := range states {
		states[index].generation++
		states[index].consecutive = 0
		states[index].interval = 0
		states[index].nextPoll = time.Time{}
	}
	s.idleBackoffs[identity] = states
}

func (s *Scheduler) pollDemandCounters(ctx context.Context, candidates []*tickCandidate, now time.Time) {
	byProvider := make(map[apiv1.Provider][]demandPoll)
	for _, candidate := range candidates {
		if candidate.schedulePollDue {
			provider := quotaProvider(candidate.entry.PollProvider)
			byProvider[provider] = append(byProvider[provider], demandPoll{
				candidate: candidate,
				counter:   candidate.entry.ScheduleDemandCounter,
				schedule:  true,
			})
		}
		if candidate.backlogPollDue {
			provider := quotaProvider(candidate.entry.PollProvider)
			byProvider[provider] = append(byProvider[provider], demandPoll{
				candidate: candidate,
				counter:   candidate.entry.BacklogCounter,
			})
		}
		if candidate.refillPollDue {
			provider := quotaProvider(candidate.entry.PollProvider)
			byProvider[provider] = append(byProvider[provider], demandPoll{
				candidate: candidate,
				counter:   candidate.entry.RefillDemandCounter,
				refill:    true,
			})
		}
	}
	providerNames := make([]string, 0, len(byProvider))
	for provider := range byProvider {
		providerNames = append(providerNames, string(provider))
	}
	sort.Strings(providerNames)

	for _, providerName := range providerNames {
		provider := apiv1.Provider(providerName)
		due := byProvider[provider]
		sort.Slice(due, func(i, j int) bool {
			left, right := due[i].candidate.entry, due[j].candidate.entry
			if left.PollPriority != right.PollPriority {
				return left.PollPriority > right.PollPriority
			}
			if left.Workflow != right.Workflow {
				return left.Workflow < right.Workflow
			}
			if left.Gaggle != right.Gaggle {
				return left.Gaggle < right.Gaggle
			}
			if due[i].schedule != due[j].schedule {
				return due[i].schedule
			}
			return due[i].refill && !due[j].refill
		})

		s.journalProviderQuotaReset(provider, now)
		pacing := s.quotaResumePacingActive(provider)
		var pacedIdentity WorkflowIdentity
		if pacing {
			for _, poll := range due {
				identity := entryIdentity(poll.candidate.entry)
				if pacedIdentity == (WorkflowIdentity{}) {
					pacedIdentity = identity
				}
			}
		}
		for _, poll := range due {
			entry := poll.candidate.entry
			if s.authCircuitOpen(entryIdentity(entry)) {
				s.applyDemandCount(poll, 0)
				continue
			}
			if pacing && entryIdentity(entry) != pacedIdentity {
				if poll.schedule {
					s.deferScheduleDemandPoll(poll.candidate)
				}
				continue
			}
			if poll.schedule {
				poll.candidate.schedulePollDue = false
			} else if poll.refill {
				poll.candidate.refillPollDue = false
				s.recordRefillPoll(entry, now)
			} else {
				poll.candidate.backlogPollDue = false
				s.recordBacklogPoll(entry, now)
			}
			decision := ProviderPollBudget{Provider: provider, Requested: 1, Allowed: 1}
			if s.providerQuota != nil {
				decision = s.providerQuota.ReservePolls(provider, now, 1)
			}
			if decision.Reset {
				s.journalProviderQuotaResetDecision(provider, decision.RemainingBefore, decision.ResetAt)
			}
			if decision.Allowed > 0 {
				pollCtx := WithProviderPollBudget(ctx, decision)
				s.applyDemandCount(poll, s.pollDemand(pollCtx, entry, poll))
				s.markPollProgress()
				continue
			}
			if guarded, ok := poll.counter.(ProviderQuotaGuardedBacklogCounter); ok && guarded.ProviderQuotaGuarded() {
				s.applyDemandCount(poll, s.pollDemand(ctx, entry, poll))
				s.markPollProgress()
				continue
			}
			s.applyDemandCount(poll, 0)
			s.journalPollShed(entry, provider, decision.RemainingBefore, len(due), decision.ResetAt)
		}
	}
}

func (s *Scheduler) deferScheduleDemandPoll(candidate *tickCandidate) {
	identity := entryIdentity(candidate.entry)
	s.persistScheduleDemand(identity, true)
	s.mu.Lock()
	s.pendingScheduleDemand[identity] = scheduledDemand{
		schedule: candidate.schedule,
		repoll:   true,
	}
	s.mu.Unlock()
}

func (s *Scheduler) paceQuotaResumedCandidates(candidates []*tickCandidate) {
	chosen := make(map[apiv1.Provider]WorkflowIdentity)
	due := make(map[apiv1.Provider]map[WorkflowIdentity]struct{})
	deferredPolls := make(map[apiv1.Provider]bool)
	for _, candidate := range candidates {
		identity := entryIdentity(candidate.entry)
		pollProvider := quotaProvider(candidate.entry.PollProvider)
		if s.quotaResumePacingActive(pollProvider) &&
			(candidate.schedulePollDue || candidate.backlogPollDue || candidate.refillPollDue) {
			if due[pollProvider] == nil {
				due[pollProvider] = make(map[WorkflowIdentity]struct{})
			}
			due[pollProvider][identity] = struct{}{}
			deferredPolls[pollProvider] = true
		}
		if candidate.scheduleRemaining == 0 && candidate.backlogRemaining == 0 && candidate.refillRemaining == 0 {
			continue
		}
		provider := quotaProvider(candidate.entry.RepoRef.Provider)
		if !s.quotaResumePacingActive(provider) {
			continue
		}
		if due[provider] == nil {
			due[provider] = make(map[WorkflowIdentity]struct{})
		}
		due[provider][identity] = struct{}{}
		allowed, ok := chosen[provider]
		if !ok {
			chosen[provider] = identity
			continue
		}
		if allowed == identity {
			continue
		}
		if candidate.scheduleRemaining > 0 && !candidate.scheduleDemand {
			s.deferScheduledDispatch(candidate)
		}
		candidate.scheduleRemaining = 0
		if candidate.backlogRemaining > 0 {
			s.clearBacklogPoll(candidate.entry)
			candidate.backlogRemaining = 0
		}
		if candidate.refillRemaining > 0 {
			s.clearRefillPoll(candidate.entry)
			candidate.refillRemaining = 0
		}
	}
	for provider, identities := range due {
		if len(identities) <= 1 && !deferredPolls[provider] {
			s.setQuotaResumePacing(provider, false)
		}
	}
	for provider := range s.activeQuotaResumeProviders() {
		if len(due[provider]) == 0 {
			s.setQuotaResumePacing(provider, false)
		}
	}
}

func (s *Scheduler) deferScheduledDispatch(candidate *tickCandidate) {
	identity := entryIdentity(candidate.entry)
	s.persistScheduleDemand(identity, true)
	s.mu.Lock()
	s.pendingScheduleDemand[identity] = scheduledDemand{
		schedule:  candidate.schedule,
		remaining: candidate.scheduleRemaining,
	}
	s.mu.Unlock()
}

func (s *Scheduler) clearBacklogPoll(entry WorkflowEntry) {
	s.mu.Lock()
	delete(s.backlogLastCheck, entryIdentity(entry))
	s.mu.Unlock()
}

func (s *Scheduler) clearRefillPoll(entry WorkflowEntry) {
	s.mu.Lock()
	delete(s.refillLastCheck, entryIdentity(entry))
	s.mu.Unlock()
}

func (s *Scheduler) applyDemandCount(poll demandPoll, ready int) {
	if poll.schedule {
		identity := entryIdentity(poll.candidate.entry)
		if !s.persistScheduleDemand(identity, ready > 0) {
			ready = 0
		}
		poll.candidate.scheduleRemaining = ready
		poll.candidate.scheduleDemand = ready > 0
		s.mu.Lock()
		if ready > 0 {
			s.pendingScheduleDemand[identity] = scheduledDemand{
				schedule:  poll.candidate.schedule,
				remaining: ready,
			}
		} else {
			delete(s.pendingScheduleDemand, identity)
		}
		s.mu.Unlock()
		return
	}
	if poll.refill {
		poll.candidate.refillEligible = ready
		return
	}
	poll.candidate.backlogRemaining = ready
}

func (s *Scheduler) consumePendingScheduleDemand(entry WorkflowEntry) {
	identity := entryIdentity(entry)
	s.mu.Lock()
	pending, ok := s.pendingScheduleDemand[identity]
	if !ok {
		s.mu.Unlock()
		return
	}
	pending.remaining--
	if pending.remaining <= 0 {
		delete(s.pendingScheduleDemand, identity)
	} else {
		s.pendingScheduleDemand[identity] = pending
	}
	s.mu.Unlock()
	s.persistScheduleDemand(identity, pending.remaining > 0)
}

func (s *Scheduler) clearPendingScheduleDemand(entry WorkflowEntry) {
	identity := entryIdentity(entry)
	s.mu.Lock()
	delete(s.pendingScheduleDemand, identity)
	s.mu.Unlock()
	s.persistScheduleDemand(identity, false)
}

func (s *Scheduler) persistScheduleDemand(identity WorkflowIdentity, outstanding bool) bool {
	state, err := readScheduleDemandState(s.log.Dir())
	if err == nil {
		if outstanding {
			state[identity] = true
		} else {
			delete(state, identity)
		}
		err = writeScheduleDemandState(s.log.Dir(), s.stateOwner, state)
	}
	if err == nil {
		return true
	}
	s.journalEvent(journal.Event{
		Type:     journal.EventError,
		Workflow: identity.Workflow,
		Gaggle:   identity.Gaggle,
		Error: &journal.ErrorDetail{
			Code:    "schedule_demand_persist_failed",
			Message: err.Error(),
		},
	})
	return false
}

// markPollProgress fires the optional WithPollHeartbeat callback after a
// provider-backed poll completes. Called from inside Tick's tickMu-held
// section (#3806), so onPollProgress itself must be cheap/non-blocking —
// see its doc comment on the Scheduler struct.
func (s *Scheduler) markPollProgress() {
	if s.onPollProgress != nil {
		s.onPollProgress(s.now())
	}
}

func (s *Scheduler) pollDemand(ctx context.Context, entry WorkflowEntry, poll demandPoll) int {
	pollCtx, cancel := context.WithTimeout(ctx, s.demandPollTimeout)
	defer cancel()
	ready, err := poll.counter.EligibleCount(pollCtx)
	if err != nil {
		var budgetErr *ProviderPollBudgetError
		if errors.As(err, &budgetErr) {
			s.journalPollShed(entry, budgetErr.Provider, budgetErr.Remaining, budgetErr.Requested, budgetErr.ResetAt)
			return 0
		}
		if providers.IsAuthenticationError(err) {
			s.openAuthCircuit(entryIdentity(entry))
			s.journalEvent(journal.Event{
				Type:     journal.EventError,
				Workflow: entry.Workflow,
				Gaggle:   entry.Gaggle,
				Error:    &journal.ErrorDetail{Code: providers.ErrorCodeAuthFailed, Message: err.Error()},
			})
			return 0
		}
		code := "backlog_count_failed"
		if poll.schedule {
			code = "schedule_demand_count_failed"
		} else if poll.refill {
			code = "refill_demand_count_failed"
		}
		s.journalEvent(journal.Event{
			Type:     journal.EventError,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Error:    &journal.ErrorDetail{Code: code, Message: err.Error()},
		})
		return 0
	}
	if ready < 0 {
		return 0
	}
	return ready
}

func (s *Scheduler) journalPollShed(entry WorkflowEntry, provider apiv1.Provider, remaining, requested int, resetAt time.Time) {
	s.journalEvent(journal.Event{
		Type:     journal.EventPollShed,
		Workflow: entry.Workflow,
		Gaggle:   entry.Gaggle,
		Reason: fmt.Sprintf(
			"%s: provider=%s priority=%d remaining=%d requested=%d reset=%s",
			ReasonProviderQuotaBudget, provider, entry.PollPriority,
			remaining, requested, resetAt.UTC().Format(time.RFC3339),
		),
	})
}

func (s *Scheduler) journalProviderQuotaReset(provider apiv1.Provider, now time.Time) {
	if s.providerQuota == nil {
		return
	}
	reset, ok := s.providerQuota.ResetIfDue(provider, now)
	if !ok {
		return
	}
	s.setQuotaResumePacing(reset.Provider, true)
	s.journalProviderQuotaResetDecision(reset.Provider, reset.Remaining, reset.ResetAt)
}

func (s *Scheduler) quotaResumePacingActive(provider apiv1.Provider) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quotaResumePacing[quotaProvider(provider)]
}

func (s *Scheduler) setQuotaResumePacing(provider apiv1.Provider, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider = quotaProvider(provider)
	if active {
		s.quotaResumePacing[provider] = true
		return
	}
	delete(s.quotaResumePacing, provider)
}

func (s *Scheduler) activeQuotaResumeProviders() map[apiv1.Provider]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	providers := make(map[apiv1.Provider]struct{}, len(s.quotaResumePacing))
	for provider := range s.quotaResumePacing {
		providers[provider] = struct{}{}
	}
	return providers
}

func (s *Scheduler) journalProviderQuotaResetDecision(provider apiv1.Provider, remaining int, resetAt time.Time) {
	s.journalEvent(journal.Event{
		Type: journal.EventProviderQuotaReset,
		Reason: fmt.Sprintf(
			"provider=%s reset=%s remaining=%d; provider budget reopened",
			provider, resetAt.UTC().Format(time.RFC3339), remaining,
		),
	})
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
	return s.TriggerWithDispatchContext(ctx, ctx, workflow, now)
}

// TriggerWithDispatchContext validates with ctx while starting an admitted run
// with dispatchCtx — Trigger's separated-lifetime form, the same shape
// TriggerSignalWithDispatchContext has had since delegated webhook triggers
// landed.
//
// It exists because dispatch() runs the Starter goroutine on the context it is
// handed (scheduler.go's dispatch), and the trigger plane calls in on
// request.Context() — which Go cancels the instant the HTTP handler returns.
// For the local runner that silently drains a trigger-plane-started run at its
// first stage boundary; for an engine-driven run, whose Starter BLOCKS on the
// workflow's Get, it would return PhaseRunning the moment the POST responded,
// release the maxConcurrentRuns slot, and skip every terminal hook while the
// workflow kept executing — silent duplicate admission (decision 005 D1,
// finding 002 "D1 BLOCKING SEMANTICS"). Both bugs are the same bug; this is
// the seam that closes it for the unqualified-name path.
func (s *Scheduler) TriggerWithDispatchContext(ctx, dispatchCtx context.Context, workflow string, now time.Time) (runID string, err error) {
	s.mu.Lock()
	var entry WorkflowEntry
	var gaggles []string
	for identity, candidate := range s.workflows {
		if identity.Workflow == workflow {
			entry = candidate
			gaggles = append(gaggles, identity.Gaggle)
		}
	}
	s.mu.Unlock()
	if len(gaggles) == 0 {
		return "", fmt.Errorf("localscheduler: unknown workflow %q", workflow)
	}
	if len(gaggles) > 1 {
		sort.Strings(gaggles)
		commands := make([]string, 0, len(gaggles))
		for _, gaggle := range gaggles {
			commands = append(commands, fmt.Sprintf("%q", "goobers run "+gaggle+"/"+workflow))
		}
		return "", fmt.Errorf(
			"localscheduler: workflow %q is ambiguous; candidate gaggles: %s; retry with %s",
			workflow, strings.Join(gaggles, ", "), strings.Join(commands, " or "),
		)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.triggerWorkflow(dispatchCtx, entry, now,
		journal.Trigger{Kind: journal.TriggerManual, Ref: entry.Workflow},
		"manual")
}

// TriggerSignal fires one unqualified workflow with an external signal
// reference, using the same ambiguity rules as Trigger.
func (s *Scheduler) TriggerSignal(ctx context.Context, workflow, signal, ref string, now time.Time) (runID string, err error) {
	return s.TriggerSignalWithDispatchContext(ctx, ctx, workflow, signal, ref, now)
}

// TriggerSignalWithDispatchContext validates with ctx while starting an
// admitted run with dispatchCtx. Delegated triggers use this to bound provider
// validation by the client request without tying the run's lifetime to that
// short-lived request.
func (s *Scheduler) TriggerSignalWithDispatchContext(ctx, dispatchCtx context.Context, workflow, signal, ref string, now time.Time) (runID string, err error) {
	s.mu.Lock()
	var gaggles []string
	for identity := range s.workflows {
		if identity.Workflow == workflow {
			gaggles = append(gaggles, identity.Gaggle)
		}
	}
	s.mu.Unlock()
	if len(gaggles) == 0 {
		return "", fmt.Errorf("localscheduler: unknown workflow %q", workflow)
	}
	if len(gaggles) > 1 {
		sort.Strings(gaggles)
		commands := make([]string, 0, len(gaggles))
		for _, gaggle := range gaggles {
			commands = append(commands, fmt.Sprintf("%q", "goobers run "+gaggle+"/"+workflow))
		}
		return "", fmt.Errorf(
			"localscheduler: workflow %q is ambiguous; candidate gaggles: %s; retry with %s",
			workflow, strings.Join(gaggles, ", "), strings.Join(commands, " or "),
		)
	}
	return s.TriggerSignalExactWithDispatchContext(ctx, dispatchCtx,
		WorkflowIdentity{Gaggle: gaggles[0], Workflow: workflow}, signal, ref, now)
}

// TriggerExact manually fires one workflow identified by its gaggle and name.
func (s *Scheduler) TriggerExact(ctx context.Context, identity WorkflowIdentity, now time.Time) (runID string, err error) {
	return s.TriggerExactWithDispatchContext(ctx, ctx, identity, now)
}

// TriggerExactWithDispatchContext is TriggerExact with separate validation and
// run-lifetime contexts. See TriggerWithDispatchContext for why the trigger
// plane and the pending-trigger sweep must use this form and not TriggerExact.
func (s *Scheduler) TriggerExactWithDispatchContext(ctx, dispatchCtx context.Context, identity WorkflowIdentity, now time.Time) (runID string, err error) {
	s.mu.Lock()
	entry, ok := s.workflows[identity]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("localscheduler: unknown workflow %q in gaggle %q", identity.Workflow, identity.Gaggle)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.triggerWorkflow(dispatchCtx, entry, now,
		journal.Trigger{Kind: journal.TriggerManual, Ref: entry.Workflow},
		"manual")
}

// TriggerSignalExact fires one exact workflow with an external signal
// reference, retaining normal run-condition admission.
func (s *Scheduler) TriggerSignalExact(ctx context.Context, identity WorkflowIdentity, signal, ref string, now time.Time) (runID string, err error) {
	return s.TriggerSignalExactWithDispatchContext(ctx, ctx, identity, signal, ref, now)
}

// TriggerSignalExactWithDispatchContext is TriggerSignalExact with separate
// validation and run-lifetime contexts.
func (s *Scheduler) TriggerSignalExactWithDispatchContext(ctx, dispatchCtx context.Context, identity WorkflowIdentity, signal, ref string, now time.Time) (runID string, err error) {
	s.mu.Lock()
	entry, ok := s.workflows[identity]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("localscheduler: unknown workflow %q in gaggle %q", identity.Workflow, identity.Gaggle)
	}
	subscribed := false
	for _, configuredSignal := range entry.Signals {
		if configuredSignal == signal {
			subscribed = true
			break
		}
	}
	if !subscribed {
		return "", fmt.Errorf("localscheduler: workflow %q in gaggle %q is not subscribed to signal %q", identity.Workflow, identity.Gaggle, signal)
	}
	if pullNumber, targeted := webhookhttp.PullNumberFromTriggerRef(ref); targeted && s.targetedPRValidator != nil {
		number, convErr := strconv.Atoi(pullNumber)
		if convErr != nil {
			return "", fmt.Errorf("localscheduler: invalid targeted pull request %q: %w", pullNumber, convErr)
		}
		if err := s.targetedPRValidator(ctx, entry, number); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if deadline, ok := ctx.Deadline(); ok && !s.now().Before(deadline) {
			return "", context.DeadlineExceeded
		}
	}
	return s.triggerWorkflow(dispatchCtx, entry, now,
		journal.Trigger{Kind: journal.TriggerSignal, Ref: ref},
		"signal")
}

// TriggerPriority immediately re-evaluates one exact workflow after a prior run
// publishes state that can change its selection order. It is an output-driven
// signal, not a bypass: normal readiness admission still applies.
func (s *Scheduler) TriggerPriority(ctx context.Context, identity WorkflowIdentity, sourceRun string, now time.Time) (runID string, err error) {
	return s.TriggerPriorityWithDispatchContext(ctx, ctx, identity, sourceRun, now)
}

// TriggerPriorityWithDispatchContext is TriggerPriority with separate
// validation and run-lifetime contexts. See TriggerWithDispatchContext.
func (s *Scheduler) TriggerPriorityWithDispatchContext(ctx, dispatchCtx context.Context, identity WorkflowIdentity, sourceRun string, now time.Time) (runID string, err error) {
	s.mu.Lock()
	entry, ok := s.workflows[identity]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("localscheduler: unknown workflow %q in gaggle %q", identity.Workflow, identity.Gaggle)
	}
	if strings.TrimSpace(sourceRun) == "" {
		return "", errors.New("localscheduler: priority trigger source run is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.triggerWorkflow(dispatchCtx, entry, now,
		journal.Trigger{Kind: journal.TriggerSignal, Ref: "priority-re-tick:" + sourceRun},
		"priority re-tick requested by run "+sourceRun)
}

func (s *Scheduler) triggerWorkflow(ctx context.Context, entry WorkflowEntry, now time.Time, trigger journal.Trigger, reason string) (runID string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tick := TickResult{Fire: true, LastEval: now}
	if reason == "" {
		reason = fireReason(tick, trigger.Kind)
	}
	if trigger.Kind == journal.TriggerSignal {
		s.resetIdleBackoff(entryIdentity(entry))
	}
	runID, admitted, skipReason := s.dispatch(ctx, entry, now, trigger, reason, nil)
	if !admitted {
		return "", &TriggerRejectedError{Workflow: entry.Workflow, Reason: skipReason}
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

// RecordRecoveredTrigger distinguishes a delegated request claimed after a
// daemon restart from a retry or resume of an existing run.
func (s *Scheduler) RecordRecoveredTrigger(requestID, workflow, runID string) {
	s.journalEvent(journal.Event{
		Type:     journal.EventRunnerAnnotation,
		Workflow: workflow,
		RunID:    runID,
		Runner: map[string]any{
			"kind":      journal.RunnerAnnotationTriggerRecovery,
			"action":    journal.RecoveryActionNewClaim,
			"requestId": requestID,
		},
	})
}

// Signal fires every workflow subscribed to the named external signal (WF-014,
// #342: a type=signal trigger declares Signal: "<name>") — `goobers signal
// <name>` CLI wiring calls this; authenticated repository webhooks use
// SignalWebhook below. Unlike Trigger (which
// names exactly one workflow and reports why it didn't start), Signal
// broadcasts to however many workflows are subscribed — zero, one, or many —
// so a conditions-driven skip is silent per subscriber (best-effort, the same
// semantics a cron Tick's skip has) rather than a caller-facing error; the
// skip is still journaled via dispatch's own tick.skipped event. Returns the
// run ids of every workflow actually admitted, in bounded-fair gaggle order
// and workflow-name order within each gaggle.
func (s *Scheduler) Signal(ctx context.Context, name string, now time.Time) []string {
	return s.signal(ctx, name, name, "signal", now, func(WorkflowEntry) bool { return true })
}

// SignalWebhook routes an authenticated GitHub delivery only to workflows for
// the repository named by the payload. Pull-request deliveries retain the PR
// number in the run trigger reference so merge-review can select it directly.
func (s *Scheduler) SignalWebhook(ctx context.Context, delivery webhookhttp.Delivery, now time.Time) []string {
	name := webhookhttp.SignalName(delivery.Event)
	return s.signal(ctx, name, webhookhttp.TriggerRef(delivery), "webhook delivery: "+delivery.Event, now, func(entry WorkflowEntry) bool {
		if delivery.RepositoryOwner == "" || delivery.RepositoryName == "" {
			return true
		}
		return entry.RepoRef.Provider == apiv1.ProviderGitHub &&
			strings.EqualFold(entry.RepoRef.Owner, delivery.RepositoryOwner) &&
			strings.EqualFold(entry.RepoRef.Name, delivery.RepositoryName)
	})
}

func (s *Scheduler) signal(ctx context.Context, name, ref, fire string, now time.Time, matches func(WorkflowEntry) bool) []string {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	ctx = providersnapshot.WithTick(ctx, now)

	s.mu.Lock()
	var subscribed []WorkflowEntry
	for _, e := range s.workflows {
		if !matches(e) {
			continue
		}
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
		s.resetIdleBackoff(entryIdentity(entry))
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
				runID, admitted, reason := s.dispatch(ctx, entry, now,
					journal.Trigger{Kind: journal.TriggerSignal, Ref: ref}, fire, nil)
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

func (s *Scheduler) evaluateRefillOpportunities(candidates []*tickCandidate, now time.Time) {
	for _, candidate := range candidates {
		entry := candidate.entry
		desired := int(entry.Readiness.DesiredConcurrentRuns)
		if desired <= 0 || entry.RefillDemandCounter == nil || candidate.refillEligible <= 0 {
			continue
		}

		identity := entryIdentity(entry)
		active := s.conditions.ActiveWorkflow(identity)
		planned := candidate.scheduleRemaining + candidate.backlogRemaining
		missing := desired - active - planned
		eligible := candidate.refillEligible - planned
		if missing <= 0 || eligible <= 0 {
			continue
		}

		s.mu.Lock()
		retryAfter, blocked := s.refillBlockedUntil[identity]
		s.mu.Unlock()

		if blocked && now.Before(retryAfter) {
			continue
		}

		candidate.refillRemaining = min(missing, eligible)
	}
}

func (s *Scheduler) nextRefillRetry(now time.Time) time.Time {
	delay := s.refillBackoff
	if s.refillBackoffJitter > 0 && s.refillRandN != nil {
		jitter := s.refillRandN(int64(s.refillBackoffJitter) + 1)
		delay += time.Duration(jitter)
	}
	return now.Add(delay)
}

func (s *Scheduler) refillRejectionReason(identity WorkflowIdentity, now time.Time, triggerReason, reason string) string {
	if triggerReason != refillTriggerReason {
		return reason
	}
	s.mu.Lock()
	s.refillBlockedUntil[identity] = s.nextRefillRetry(now)
	s.mu.Unlock()
	return refillBlockedReasonPrefix + reason
}

// dispatch admits and starts (or skips) one due firing of entry. The caller
// supplies both the run's pinned trigger identity and the human-readable
// instance-journal reason. It returns the dispatched run's id (empty if
// skipped), whether it was admitted, and — when not admitted — the
// run-conditions skip reason.
//
// The telemetry span it opens covers only the decision (trigger -> admit/skip),
// not the run itself: entry.Starter.Start runs in its own
// goroutine below and outlives dispatch's return, so the run gets its own
// root span (via runner.Runner.startRunSpan). The candidate run ID is minted
// first so both spans share its trace even when admission blocks the dispatch.
func (s *Scheduler) dispatch(ctx context.Context, entry WorkflowEntry, now time.Time, trigger journal.Trigger, triggerReason string, scheduleIndexes []int) (runID string, admitted bool, skipReason string) {
	ctx = providersnapshot.WithTick(ctx, now)
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

	identity := entryIdentity(entry)
	if s.authCircuitOpen(identity) {
		reason := ReasonProviderAuth + ": operator must repair credentials and reload configuration"
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason:   s.refillRejectionReason(identity, now, triggerReason, reason),
		})
		span.Complete(telemetry.OutcomeBlocked, false)
		return "", false, reason
	}

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
		Reason:   triggerReason,
	}); err != nil {
		reason := "trigger.fired journal write failed: " + err.Error()
		span.Fail(err)
		return "", false, reason
	}

	// Checkpoint-3 refusal (#2860, dsl-3.0.md §5): a workflow the startup
	// constraint solve marked unplaceable on the declared inventory is refused
	// per run with the solver's named diagnostic — the proportionate
	// replacement for the boot-kill this ruling removed. Permanent for the
	// pinned inventory (restart-only, accept-and-pin), so not transient.
	if entry.PlacementRefusal != "" {
		reason := ReasonPlacementUnsatisfiable + ": " + entry.PlacementRefusal
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason:   s.refillRejectionReason(identity, now, triggerReason, reason),
		})
		span.Complete(telemetry.OutcomeBlocked, false)
		return "", false, reason
	}
	// Schedule-time runner-capability match (RRQ-1/#1101): refuse the run
	// before it can consume an admission slot when the runner does not satisfy
	// a capability the workflow's gaggle/stages require. This is the runtime,
	// per-run enforcement of the same invariant checkpoint 1 validates
	// statically, served from the shared solver's self-runner view (#3506) so
	// the two can never diverge — a missing claim fails a run to schedule
	// rather than scheduling it to fail at run. Placement across a
	// multi-runner inventory is dispatch-time work (#3513); until it lands,
	// every stage of an admitted run executes on this host, which is exactly
	// what this self-runner check answers for.
	if missing := s.selfRunner.MissingCapabilities(entry.RequiredCapabilities); len(missing) > 0 {
		reason := ReasonMissingCapability + ": " + strings.Join(missing, ", ")
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason:   s.refillRejectionReason(identity, now, triggerReason, reason),
		})
		span.Complete(telemetry.OutcomeBlocked, false)
		return "", false, reason
	}
	provider := quotaProvider(entry.RepoRef.Provider)
	s.journalProviderQuotaReset(provider, now)
	ok, reason := s.conditions.AdmitProviderWorkflow(identity, provider, entry.Readiness, now)
	if !ok {
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason:   s.refillRejectionReason(identity, now, triggerReason, reason),
		})
		span.Complete(telemetry.OutcomeBlocked, false)
		return "", false, reason
	}
	if triggerReason == refillTriggerReason {
		s.mu.Lock()
		delete(s.refillBlockedUntil, identity)
		s.mu.Unlock()
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

	s.admissionMu.Lock()
	s.mu.Lock()
	s.nextAdmissionGeneration++
	admissionGeneration := s.nextAdmissionGeneration
	s.admittedRuns[runID] = runAdmission{
		identity:   identity,
		generation: admissionGeneration,
		owners:     1,
	}
	s.mu.Unlock()
	s.admissionMu.Unlock()
	s.dispatches.Add(1)
	backoffTokens := s.beginScheduledPoll(identity, scheduleIndexes)
	go func() {
		defer s.dispatches.Done()
		defer s.releaseAdmissionOwner(runID, entry.Workflow, admissionGeneration)
		entry.Starter = gooberDigestStarter{digest: entry.GooberDigest, next: entry.Starter}
		result, startErr := entry.Starter.Start(ctx, StartRequest{
			RunID:   runID,
			Gaggle:  entry.Gaggle,
			Trigger: trigger,
			RepoRef: entry.RepoRef,
		})
		if startErr == nil {
			s.recordScheduledPollResult(identity, entry, backoffTokens, result.NoWork, s.now())
		}
		if (startErr == nil && result.FailureCode == providers.ErrorCodeAuthFailed) ||
			result.FailureCode == telemetry.ErrCodeCredentialUnavailable {
			s.openAuthCircuit(identity)
		}
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

func (s *Scheduler) authCircuitOpen(identity WorkflowIdentity) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, open := s.authCircuits[identity]
	return open
}

func (s *Scheduler) openAuthCircuit(identity WorkflowIdentity) {
	s.mu.Lock()
	s.authCircuits[identity] = struct{}{}
	s.mu.Unlock()
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

// RefillBlockedReason returns the embedded admission condition from a refill
// rejection event reason.
func RefillBlockedReason(reason string) (string, bool) {
	if !strings.HasPrefix(reason, refillBlockedReasonPrefix) {
		return "", false
	}
	blocking := strings.TrimSpace(strings.TrimPrefix(reason, refillBlockedReasonPrefix))
	if blocking == "" {
		return "", false
	}
	return blocking, true
}

// IsRefillTriggerReason reports whether a trigger event was created by the
// desired-concurrency refill path.
func IsRefillTriggerReason(reason string) bool {
	return reason == refillTriggerReason
}

// nextWakeup computes how long to sleep until the earliest workflow trigger or
// desired-concurrency eligibility poll is next due, so Run idles instead of
// busy-polling. A workflow with no schedule, backlog counter, or refill counter
// does not contribute; if none are managed, it returns a conservative default
// so the loop still wakes periodically for Reconcile-style housekeeping rather
// than blocking forever.
func (s *Scheduler) nextWakeup(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.quotaResumePacing) > 0 {
		return minPoll
	}
	var earliest time.Time
	consider := func(next time.Time) {
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
		}
	}
	for name, entry := range s.workflows {
		if _, open := s.authCircuits[name]; open {
			continue
		}
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
		if entry.RefillDemandCounter != nil && entry.Readiness.DesiredConcurrentRuns > 0 {
			next := s.refillLastCheck[name].Add(backlogPollInterval)
			if retryAfter := s.refillBlockedUntil[name]; retryAfter.After(next) {
				next = retryAfter
			}
			consider(next)
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

// journalPlacementRefusals records one workflow.refused event per entry the
// startup constraint solve marked unplaceable (#2860, dsl-3.0.md §5
// checkpoint 3). Called when the scheduler learns a configuration — New and
// Reload — so the instance journal and `goobers status` name every refusal
// without waiting for a dispatch attempt. Best-effort like every other
// decision record (the refusal is enforced by dispatch regardless).
func (s *Scheduler) journalPlacementRefusals(entries []WorkflowEntry) {
	for _, entry := range entries {
		if entry.PlacementRefusal == "" {
			continue
		}
		s.journalEvent(journal.Event{
			Type:     journal.EventWorkflowRefused,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Reason:   entry.PlacementRefusal,
		})
	}
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
