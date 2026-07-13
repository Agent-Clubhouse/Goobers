package localscheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// WorkflowEntry is one workflow the scheduler manages: its readiness
// conditions, its schedule trigger (nil for a manual-only workflow — V0 acts
// only on schedule-type triggers per §7; backlog-item consumption is itself a
// cron-triggered workflow whose first stage claims items), the Starter that
// dispatches a run, and the repo every run's stages branch worktrees from.
type WorkflowEntry struct {
	Workflow  string
	Gaggle    string
	Readiness apiv1.ReadinessConditions
	Schedule  Schedule
	Starter   Starter
	RepoRef   apiv1.RepoRef
}

// minPoll floors the computed sleep-until-next-tick duration, so a schedule
// that just fired (Next() a few nanoseconds out due to clock jitter) can't spin
// the loop.
const minPoll = time.Second

// newRunID is the run-id generator; swappable in tests for determinism.
var newRunID = telemetry.NewRunID

// Scheduler is the embedded scheduler daemon (§7, SCH-001): it ties cron
// evaluation, run conditions, and the Starter seam together into one
// idle-between-ticks loop, journaling every decision to the instance journal.
type Scheduler struct {
	workflows  map[string]WorkflowEntry
	conditions *Conditions
	log        *journal.InstanceLog
	now        func() time.Time
	after      func(d time.Duration) <-chan time.Time

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
		ts := TriggerState{Workflow: e.Workflow, Schedule: e.Schedule, LastEval: s.now()}
		s.triggers[e.Workflow] = ts
	}
	return s
}

// Reconcile seeds Conditions' active-run counts from runsDir and each
// workflow's trigger LastEval from the instance journal's trigger.fired
// history — the daemon-restart recovery pass. Call once before Run.
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
	for _, ev := range events {
		if ev.Type == journal.EventTriggerFired {
			fired = append(fired, TriggerFiredRecord{Workflow: ev.Workflow, Time: ev.Time})
		}
	}

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
		if entry.Schedule == nil {
			continue // manual-only workflow: not cron-managed
		}
		s.mu.Lock()
		ts := s.triggers[entry.Workflow]
		s.mu.Unlock()

		res := Tick(ts, now)
		s.mu.Lock()
		s.triggers[entry.Workflow] = TriggerState{Workflow: entry.Workflow, Schedule: entry.Schedule, LastEval: res.LastEval}
		s.mu.Unlock()
		if !res.Fire {
			continue
		}
		s.dispatch(ctx, entry, now, res)
	}
}

// Trigger manually fires workflow now, bypassing its cron schedule but still
// honoring run conditions (SCH-002/#23's `goobers run <workflow>` CLI wiring
// calls this). Returns an error only if the workflow is unknown; a
// conditions-driven skip is not an error, it's journaled like any other skip.
func (s *Scheduler) Trigger(ctx context.Context, workflow string, now time.Time) error {
	s.mu.Lock()
	entry, ok := s.workflows[workflow]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("localscheduler: unknown workflow %q", workflow)
	}
	s.dispatch(ctx, entry, now, TickResult{Fire: true, LastEval: now})
	return nil
}

// dispatch admits and starts (or skips) one due firing of entry.
func (s *Scheduler) dispatch(ctx context.Context, entry WorkflowEntry, now time.Time, tick TickResult) {
	s.journalEvent(journal.Event{
		Type:     journal.EventTriggerFired,
		Workflow: entry.Workflow,
		Reason:   fireReason(tick),
	})

	ok, reason := s.conditions.Admit(entry.Workflow, entry.Readiness, now)
	if !ok {
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Reason:   reason,
		})
		return
	}

	runID, err := newRunID()
	if err != nil {
		s.conditions.Release(entry.Workflow)
		s.journalEvent(journal.Event{
			Type:     journal.EventTickSkipped,
			Workflow: entry.Workflow,
			Reason:   "run-id generation failed: " + err.Error(),
		})
		return
	}

	s.journalEvent(journal.Event{
		Type:     journal.EventRunStarted,
		Workflow: entry.Workflow,
		RunID:    runID,
	})

	go func() {
		defer s.conditions.Release(entry.Workflow)
		result, startErr := entry.Starter.Start(ctx, StartRequest{
			RunID:   runID,
			Gaggle:  entry.Gaggle,
			Trigger: journal.Trigger{Kind: journal.TriggerSchedule, Ref: entry.Workflow},
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
}

// fireReason renders a stable reason string for a trigger.fired event.
func fireReason(tick TickResult) string {
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
		if entry.Schedule == nil {
			continue
		}
		ts := s.triggers[name]
		next := entry.Schedule.Next(ts.LastEval)
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
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
	if s.log == nil {
		return
	}
	_ = s.log.Append(ev)
}
