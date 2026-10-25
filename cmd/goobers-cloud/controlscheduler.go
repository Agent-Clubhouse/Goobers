package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/cloud/childengine"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// gateStatus records the scheduler gates this controller does NOT enforce, so an
// operator sees them as a readiness check and not only in a log line. A gate is
// disabled when the provider-backed counter that feeds it is not composed (no
// opener for the provider yet, no claims reader): the workflows that need it stay
// disabled with their pending reason, and MaxOpenPRs fails open.
type gateStatus struct {
	mu      sync.Mutex
	reasons map[string]string
}

func newGateStatus() *gateStatus { return &gateStatus{reasons: map[string]string{}} }

// Set marks a gate as not enforced.
func (g *gateStatus) Set(name, reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reasons[name] = reason
}

// Clear forgets every recorded gate (an epoch starts from nothing).
func (g *gateStatus) Clear() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reasons = map[string]string{}
}

// Checks maps "scheduler-gate:<name>" to false for every gate not enforced, for
// httpapi.ReadinessStatus.Checks.
func (g *gateStatus) Checks() map[string]bool {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.reasons) == 0 {
		return nil
	}
	out := make(map[string]bool, len(g.reasons))
	for name := range g.reasons {
		out["scheduler-gate:"+name] = false
	}
	return out
}

// Reasons returns the recorded reasons, sorted by gate name.
func (g *gateStatus) Reasons() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	names := make([]string, 0, len(g.reasons))
	for name := range g.reasons {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+": "+g.reasons[name])
	}
	return out
}

// generationCounters is one applied generation's provider-backed counters
// (schedcounters.Counters satisfies it).
type generationCounters interface {
	For(localscheduler.WorkflowIdentity) schedentries.Counters
	OpenPRs() localscheduler.OpenPRCounter
	// Run blocks until ctx ends and stops everything it started.
	Run(ctx context.Context) error
}

// schedulerComposition holds the process-wide parts of the control role's
// scheduler admission; admission() builds the per-epoch Admission from them.
type schedulerComposition struct {
	// Applied reports the running configuration generation.
	Applied func() (cloudconfig.Candidate, bool)
	// OpenLog opens the epoch's scheduler journal on the epoch context.
	// The log is shared with the terminal hooks (one open per epoch): it is NOT
	// closed here, its opener retires it when the epoch ends.
	OpenLog func(ctx context.Context, deps control.AdmissionDeps, report func(error)) (journal.InstanceStore, error)
	// State binds the scheduler state to the epoch's lease.
	State func(deps control.AdmissionDeps) (localscheduler.SchedulerState, error)
	// OpenRuns lists the runs holding a scheduler slot (open runs and
	// predecessor starts whose outcome is still uncertain).
	OpenRuns func(ctx context.Context, deps control.AdmissionDeps) ([]schedstart.OpenRun, error)
	// Terminal waits for a run to close.
	Terminal schedstart.Terminal
	// Entries builds the scheduler entries of an applied generation. nil uses
	// schedentries.Build.
	Entries func(cloudconfig.Candidate, generationCounters) ([]localscheduler.WorkflowEntry, error)
	// Counters builds one generation's counters over the shared quota state. nil
	// (or ErrCountersDisabled) leaves every provider-backed gate unenforced.
	Counters func(cloudconfig.Candidate, *localscheduler.ProviderQuotaState) (generationCounters, error)
	// Sources is the schedule/occurrence intake over cloud acceptance, built for
	// the epoch's store; nil leaves scheduled sources off.
	Sources func(deps control.AdmissionDeps, build func(context.Context, startintent.Target) (startintent.Prepared, error)) (localscheduler.SourceQueue, error)
	// Capacity receives each epoch's parent capacity (the scheduler's slots for
	// parked child workflow parents); nil publishes nothing.
	Capacity *parentCapacity
	// SlotsFor wraps the epoch's scheduler as the capacity's ParentSlots (tests
	// observe which scheduler a Park reaches); nil uses the scheduler itself.
	SlotsFor func(*localscheduler.Scheduler) childengine.ParentSlots
	// Quota is the ONE provider quota state of the process. The scheduler gates
	// on it, the counters feed it, and the observations ledger persists it.
	Quota *localscheduler.ProviderQuotaState
	// Acquire pins a retained generation for as long as a prepared start needs it.
	Acquire func(ctx context.Context, generation string) (release func(), err error)
	// Settled reports whether an uncertain start's acceptance ended without a run
	// (see schedstart.Config.Settled). nil reads the epoch's acceptance store.
	Settled func(ctx context.Context, deps control.AdmissionDeps, acceptanceID string) (bool, error)
	// Queue is the acceptance custody the drain pages and dispatches through; nil
	// uses the epoch's acceptance store.
	Queue func(control.AdmissionDeps) acceptanceQueue
	// Gates receives the scheduler gates that are not enforced.
	Gates  *gateStatus
	Now    func() time.Time
	Report func(error)
	// Options are extra scheduler options (clock injection in tests).
	Options []localscheduler.Option
}

// acceptanceQueue is the acceptance custody the admission drains.
type acceptanceQueue interface {
	schedstart.Pager
	triggerqueue.OrdinaryStore
}

// ErrCountersDisabled is what a Counters factory returns when the provider
// counters are not composed yet (for example no opener); it is reported as a
// visible disabled gate rather than treated as a failure.
var ErrCountersDisabled = errors.New("provider-backed counters are not composed")

func (c schedulerComposition) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c schedulerComposition) report(err error) {
	if err != nil && c.Report != nil {
		c.Report(err)
	}
}

// admission is the control.Runtime.Admission factory. ctx is the epoch context:
// every goroutine the admission starts derives from it, never from a Tick's.
func (c schedulerComposition) admission(ctx context.Context, deps control.AdmissionDeps) (control.Admission, error) {
	if c.Applied == nil || c.OpenLog == nil || c.State == nil || c.OpenRuns == nil || c.Terminal == nil || c.Quota == nil {
		return nil, errors.New("cloud scheduler admission requires applied configuration, log, state, open runs, terminal and quota")
	}
	if c.Entries == nil {
		return nil, errors.New("cloud scheduler admission requires an entry builder")
	}
	epoch, cancel := context.WithCancel(ctx)
	return &schedulerAdmission{c: c, epoch: epoch, cancel: cancel, deps: deps}, nil
}

// schedulerAdmission is one epoch's scheduler-backed admission.
type schedulerAdmission struct {
	c      schedulerComposition
	epoch  context.Context
	cancel context.CancelFunc
	deps   control.AdmissionDeps

	tracker schedstart.Tracker

	mu         sync.Mutex
	log        journal.InstanceStore
	scheduler  *localscheduler.Scheduler
	generation string
	entries    map[localscheduler.WorkflowIdentity]localscheduler.WorkflowEntry
	counters   generationCounters
	stopCount  context.CancelFunc
	countDone  chan struct{}
	after      string
	prepared   bool
	disabled   bool
	capacity   *childengine.SchedulerCapacity
	warned     map[string]bool
}

func (a *schedulerAdmission) starterConfig() schedstart.Config {
	return schedstart.Config{
		Engine: a.deps.Starter, Resolve: a.deps.Resolve, Terminal: a.c.Terminal,
		StartTimeout: 10 * time.Second, Report: a.c.report,
		Settled: func(ctx context.Context, acceptanceID string) (bool, error) {
			return a.c.settled(ctx, a.deps, acceptanceID)
		},
	}
}

// Prepare rebuilds admission from durable state before the epoch serves:
// the scheduler is constructed over the applied generation, reconciled against
// the runs holding slots, and their slots are released as they close.
func (a *schedulerAdmission) Prepare(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prepared {
		return errors.New("cloud scheduler admission is already prepared")
	}
	applied, ok := a.c.Applied()
	if !ok {
		return errors.New("cloud scheduler admission requires an applied configuration generation")
	}
	log, err := a.c.OpenLog(a.epoch, a.deps, a.c.report)
	if err != nil {
		return err
	}
	a.log = log
	state, err := a.c.State(a.deps)
	if err != nil {
		return err
	}
	state = localscheduler.SkewTolerantState{SchedulerState: state}
	counters, cause, err := a.buildCounters(applied)
	if err != nil {
		return err
	}
	entries, err := a.c.Entries(applied, counters)
	if err != nil {
		return err
	}
	a.setEntries(applied.Digest, entries)

	// One listing feeds both the reconcile and the release watchers, so the two
	// can never disagree about which runs hold slots.
	open, err := a.c.OpenRuns(ctx, a.deps)
	if err != nil {
		return err
	}
	options := []localscheduler.Option{
		localscheduler.WithSchedulerState(state),
		localscheduler.WithProviderQuota(a.c.Quota),
		localscheduler.WithRunDirReconciler(schedstart.NewReconciler(func(context.Context) ([]schedstart.OpenRun, error) { return open, nil })),
	}
	if applied.Config != nil {
		rc := applied.Config.RunConditions
		options = append(options, localscheduler.WithInstanceRunConditions(rc.MaxParallelRuns, rc.WorkflowBudgets, rc.WorkflowDailyBudgets))
	}
	if counters != nil {
		options = append(options, localscheduler.WithOpenPRCounter(counters.OpenPRs()))
	}
	if a.c.Sources != nil {
		queue, err := a.c.Sources(a.deps, a.build)
		if err != nil {
			return err
		}
		if queue != nil {
			options = append(options, localscheduler.WithSourceQueue(queue))
		}
	}
	options = append(options, a.c.Options...)
	a.scheduler = localscheduler.New(entries, log, options...)
	if err := a.scheduler.Reconcile("", a.c.now()); err != nil {
		return err
	}
	a.commitGates(counters, cause)
	a.publishCapacity()
	a.starterConfig().WatchReconciled(a.epoch, open, a.scheduler.ReleaseReconciled, &a.tracker)
	a.startCounters(counters)
	a.prepared = true
	return nil
}

// buildCounters builds the generation's counters over the shared quota. When
// they are not composed it returns nil counters and the cause: the caller
// records the gates (commitGates) only once the generation is actually adopted,
// so readiness never describes a generation the scheduler did not take.
func (a *schedulerAdmission) buildCounters(applied cloudconfig.Candidate) (generationCounters, error, error) {
	if a.c.Counters == nil {
		return nil, ErrCountersDisabled, nil
	}
	counters, err := a.c.Counters(applied, a.c.Quota)
	switch {
	case errors.Is(err, ErrCountersDisabled):
		return nil, err, nil
	case err != nil:
		return nil, nil, err
	}
	return counters, nil, nil
}

// commitGates makes the gate status describe the adopted generation: disabled
// gates (with one journaled warning per epoch) or none.
func (a *schedulerAdmission) commitGates(counters generationCounters, cause error) {
	if counters == nil && cause != nil {
		a.markDisabled(cause)
		return
	}
	a.clearDisabled()
	// Composed counters can still leave a gate unenforced (schedule-demand
	// without the claims plane): keep exactly those reported.
	if reporter, ok := counters.(gateReporter); ok && a.c.Gates != nil {
		for gate, reason := range reporter.DisabledGates() {
			a.c.Gates.Set(gate, reason)
			a.warnGate(gate, reason)
		}
	}
}

const providerGatesReason = "provider-backed counters are not composed (no provider opener); backlog, demand and refill workflows stay disabled and MaxOpenPRs fails open"

func (a *schedulerAdmission) markDisabled(cause error) {
	if a.c.Gates != nil {
		for _, gate := range []string{"max-open-prs", "backlog", "schedule-demand", "refill"} {
			a.c.Gates.Set(gate, providerGatesReason)
		}
	}
	if !a.disabled {
		// One journaled warning per epoch, not only a log line.
		a.disabled = true
		a.log.AppendBestEffort(journal.Event{
			Type: journal.EventRunnerAnnotation, Name: "scheduler.gates-not-enforced",
			Reason: fmt.Sprintf("%s: %v", providerGatesReason, cause),
			Runner: map[string]any{"emitKey": fmt.Sprintf("scheduler.gates-not-enforced/epoch-%d", a.deps.Handle.Epoch)},
		})
		a.c.report(fmt.Errorf("scheduler gates not enforced: %s", providerGatesReason))
	}
}

func (a *schedulerAdmission) clearDisabled() {
	if a.c.Gates != nil {
		a.c.Gates.Clear()
	}
}

func (a *schedulerAdmission) setEntries(generation string, entries []localscheduler.WorkflowEntry) {
	a.generation = generation
	a.entries = make(map[localscheduler.WorkflowIdentity]localscheduler.WorkflowEntry, len(entries))
	for _, entry := range entries {
		a.entries[localscheduler.WorkflowIdentity{Gaggle: entry.Gaggle, Workflow: entry.Workflow}] = entry
	}
}

// startCounters runs counters under a child of the epoch context.
func (a *schedulerAdmission) startCounters(counters generationCounters) {
	a.counters = counters
	if counters == nil {
		return
	}
	ctx, cancel := context.WithCancel(a.epoch)
	done := make(chan struct{})
	a.stopCount, a.countDone = cancel, done
	go func() {
		defer close(done)
		if err := counters.Run(ctx); err != nil && ctx.Err() == nil {
			a.c.report(fmt.Errorf("scheduler counters: %w", err))
		}
	}()
}

func (a *schedulerAdmission) stopCounters() {
	if a.stopCount != nil {
		a.stopCount()
		<-a.countDone
		a.stopCount, a.countDone, a.counters = nil, nil, nil
	}
}

// Tick follows a generation change, runs one scheduler pass, and admits the
// accepted rows. ctx bounds the pass only; nothing started here outlives the
// epoch context.
func (a *schedulerAdmission) Tick(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.prepared {
		return errors.New("cloud scheduler admission ticked before it was prepared")
	}
	if err := a.follow(ctx); err != nil {
		return err
	}
	a.scheduler.Tick(ctx, a.c.now())
	next, err := a.drain().Run(a.epoch, a.after)
	a.after = next
	return err
}

// follow reloads the scheduler when the applied generation changed.
func (a *schedulerAdmission) follow(context.Context) error {
	applied, ok := a.c.Applied()
	if !ok || applied.Digest == a.generation {
		return nil
	}
	counters, cause, err := a.buildCounters(applied)
	if err != nil {
		return err
	}
	entries, err := a.c.Entries(applied, counters)
	if err != nil {
		return err
	}
	var prs localscheduler.OpenPRCounter
	if counters != nil {
		prs = counters.OpenPRs()
	}
	old := a.generation
	if err := a.scheduler.Reload(entries, prs, a.c.now(), old, applied.Digest, applied.Digest); err != nil {
		return err
	}
	a.commitGates(counters, cause)
	a.stopCounters()
	a.setEntries(applied.Digest, entries)
	a.startCounters(counters)
	return nil
}

func (a *schedulerAdmission) queue() acceptanceQueue {
	if a.c.Queue != nil {
		return a.c.Queue(a.deps)
	}
	return a.deps.Store
}

func (a *schedulerAdmission) drain() schedstart.Drain {
	queue := a.queue()
	return schedstart.Drain{
		Pager: queue, RejectAccepted: a.deps.Store.RejectAccepted,
		Service: startintent.Service{
			Queue: queue, Now: a.c.now,
			Build:        a.build,
			Scheduler:    func() *localscheduler.Scheduler { return a.scheduler },
			RunDirectory: func(context.Context, string) (string, error) { return "", nil },
		},
		Config: a.starterConfig(), Tracker: &a.tracker, Report: a.c.report,
	}
}

// build prepares the archived entry of one pinned target. PreparedEntry
// requires the prepared repository to equal the current entry's, and takes the
// current readiness, so the entry is the current one re-pinned to the target's
// digests, holding the target generation's archive lease until released.
func (a *schedulerAdmission) build(ctx context.Context, target startintent.Target) (startintent.Prepared, error) {
	identity := localscheduler.WorkflowIdentity{Gaggle: target.Gaggle, Workflow: target.Workflow}
	entry, ok := a.entries[identity]
	if !ok {
		return startintent.Prepared{}, fmt.Errorf("workflow %s/%s is not configured", target.Gaggle, target.Workflow)
	}
	release := func() {}
	if a.c.Acquire != nil {
		held, err := a.c.Acquire(ctx, target.ConfigGeneration)
		if err != nil {
			return startintent.Prepared{}, err
		}
		release = held
	}
	entry.ConfigGeneration = target.ConfigGeneration
	entry.WorkflowDigest = target.WorkflowDigest
	entry.GooberDigest = target.GooberDigest
	return startintent.Prepared{Entry: entry, Release: release}, nil
}

// Stop ends everything the admission started and waits for it. It is safe when
// Prepare never ran (or failed part-way) and when called more than once.
func (a *schedulerAdmission) Stop(ctx context.Context) error {
	a.cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retireCapacity()
	a.stopCounters()
	var errs []error
	if a.scheduler != nil {
		done := make(chan struct{})
		go func() { a.scheduler.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}
	if err := a.tracker.Wait(ctx); err != nil {
		errs = append(errs, err)
	}
	// The scheduler log is shared per epoch (epochSchedulerLogs) and retired by its
	// opener when the epoch context ends; closing it here would break the terminal
	// hooks' later appends.
	a.log = nil
	return errors.Join(errs...)
}

var _ control.Admission = (*schedulerAdmission)(nil)

// uncertainStarts and the helpers below turn acceptance custody into slot holders.
func holderFromRecord(record triggerqueue.Record) (schedstart.OpenRun, bool) {
	envelope, err := startintent.Parse(record.Payload)
	if err != nil {
		return schedstart.OpenRun{}, false
	}
	return schedstart.OpenRun{
		Identity:     localscheduler.WorkflowIdentity{Gaggle: envelope.Target.Gaggle, Workflow: envelope.Target.Workflow},
		RunID:        strings.TrimPrefix(record.ID, "trigger-"),
		AcceptanceID: record.ID, // an uncertain start: NotFound from the engine is not "gone"
	}, true
}

// settled reads the acceptance of an uncertain start: it ended without a run
// (release the slot) when the row is gone, rejected, or requeued as accepted; it
// is still live ('dispatching', or 'dispatched' with a run the engine will know)
// otherwise, and the slot is held.
func (c schedulerComposition) settled(ctx context.Context, deps control.AdmissionDeps, acceptanceID string) (bool, error) {
	if c.Settled != nil {
		return c.Settled(ctx, deps, acceptanceID)
	}
	record, err := deps.Store.RecordByID(ctx, acceptanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch record.State {
	case triggerqueue.Accepted, triggerqueue.Rejected:
		return true, nil
	default: // Dispatching, Dispatched
		return false, nil
	}
}

// publishCapacity makes this epoch's scheduler the parent capacity plane. The
// capacity is built per epoch with the scheduler: a SchedulerCapacity caches the
// suspension of each parked parent, bound to the scheduler that made it, so a
// shared capacity would carry the deposed epoch's suspensions into its successor.
func (a *schedulerAdmission) publishCapacity() {
	if a.c.Capacity == nil {
		return
	}
	var slots childengine.ParentSlots = a.scheduler
	if a.c.SlotsFor != nil {
		slots = a.c.SlotsFor(a.scheduler)
	}
	a.capacity = &childengine.SchedulerCapacity{Slots: slots}
	a.c.Capacity.publish(a.capacity)
}

// retireCapacity withdraws this epoch's capacity, but only if it is still the
// published one: a deposed epoch's late Stop must not clear its successor's.
func (a *schedulerAdmission) retireCapacity() {
	if a.c.Capacity != nil && a.capacity != nil {
		a.c.Capacity.retire(a.capacity)
	}
	a.capacity = nil
}

// warnGate journals ONE warning per epoch for a gate that is composed but not
// enforced, besides marking it on the readiness checks.
func (a *schedulerAdmission) warnGate(gate, reason string) {
	if a.warned == nil {
		a.warned = map[string]bool{}
	}
	if a.warned[gate] || a.log == nil {
		return
	}
	a.warned[gate] = true
	a.log.AppendBestEffort(journal.Event{
		Type: journal.EventRunnerAnnotation, Name: "scheduler.gate-not-enforced",
		Reason: fmt.Sprintf("%s: %s", gate, reason),
		Runner: map[string]any{"emitKey": fmt.Sprintf("scheduler.gate-not-enforced/%s/epoch-%d", gate, a.deps.Handle.Epoch)},
	})
	a.c.report(fmt.Errorf("scheduler gate %s not enforced: %s", gate, reason))
}
