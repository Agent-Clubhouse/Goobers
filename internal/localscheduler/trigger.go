package localscheduler

import "time"

const (
	triggerReasonScheduled     = "scheduled"
	triggerReasonCatchUpPrefix = "catch-up "
)

// TriggerState tracks one workflow's cron evaluation state: its schedules
// (#341 — a workflow may declare more than one) and the last instant it was
// evaluated, so a tick can decide whether any of them is due. All schedules
// for a workflow share one LastEval baseline rather than being tracked
// independently — see Tick's doc comment for why.
type TriggerState struct {
	Workflow  string
	Schedules []Schedule
	// LastEval is the last instant this trigger was evaluated (fired or not).
	// It must be initialized explicitly — by the daemon's start time for a
	// trigger never seen before, or by the timestamp of its most recent
	// scheduled trigger.fired instance-journal event on restart (see
	// ReconstructLastEval) — so missed-tick catch-up is well-defined rather
	// than an epoch backfill.
	LastEval time.Time
}

// TickResult is the outcome of evaluating one TriggerState at now.
type TickResult struct {
	Fire     bool
	LastEval time.Time // the LastEval to record for the next tick
	// CatchUp is true when Fire is true because the daemon was down across one
	// or more scheduled fires — the missed-tick policy collapses any number of
	// missed fires into exactly one catch-up run (no backfill queue).
	CatchUp bool
	// MissedTicks estimates how many scheduled fires were skipped by the
	// catch-up collapse (informational; 0 when CatchUp is false). Capped at
	// maxMissedTickCount to bound the counting loop.
	MissedTicks int
}

// maxMissedTickCount bounds how many Next() calls Tick will make while
// estimating a catch-up's missed-tick count, so a pathological schedule (or a
// very long daemon outage against a fine-grained schedule) can't spin.
const maxMissedTickCount = 10_000

// Tick evaluates whether any of t.Schedules is due at now and returns the
// decision plus the LastEval to record afterward. The missed-tick policy is
// "fire at most one catch-up": however many scheduled fires fell inside
// [LastEval, now) — across every one of t.Schedules combined — Tick fires
// the workflow once and advances LastEval to now — never to the next unfired
// tick — so no backlog of stale fires replays. An empty t.Schedules never
// fires (a manual-only trigger state).
func Tick(t TriggerState, now time.Time) TickResult {
	due := false
	for _, sched := range t.Schedules {
		if !sched.Next(t.LastEval).After(now) {
			due = true
			break
		}
	}
	if !due {
		return TickResult{Fire: false, LastEval: t.LastEval}
	}

	// Due. Count how many fires this collapses across every schedule
	// (bounded per schedule) purely for the observability of the catch-up
	// reason; the decision itself is already made. A single schedule's count
	// is identical to the pre-#341 single-schedule behavior.
	missed := 0
	for _, sched := range t.Schedules {
		cursor := t.LastEval
		for i := 0; i < maxMissedTickCount; i++ {
			n := sched.Next(cursor)
			if n.After(now) {
				break
			}
			cursor = n
			missed++
		}
	}

	return TickResult{
		Fire:        true,
		LastEval:    now,
		CatchUp:     missed > 1,
		MissedTicks: missed,
	}
}

// ReconstructLastEval derives the LastEval baseline for each workflow's trigger
// after a restart, from the instance journal's scheduled trigger.fired
// history: the most recent scheduled fire time for a workflow, or startedAt
// for a workflow never observed (no epoch backfill — a trigger the daemon has
// never evaluated starts counting from daemon start, not from year zero).
func ReconstructLastEval(fired []TriggerFiredRecord, workflows []WorkflowIdentity, startedAt time.Time) map[WorkflowIdentity]time.Time {
	last := make(map[WorkflowIdentity]time.Time, len(workflows))
	seen := make(map[WorkflowIdentity]bool, len(workflows))
	for _, identity := range workflows {
		last[identity] = startedAt
	}
	for _, f := range fired {
		identity, ok := resolveWorkflowIdentity(f.Gaggle, f.Workflow, workflows)
		if !ok {
			continue
		}
		if !seen[identity] || f.Time.After(last[identity]) {
			last[identity] = f.Time
			seen[identity] = true
		}
	}
	return last
}

func resolveWorkflowIdentity(gaggle, workflow string, workflows []WorkflowIdentity) (WorkflowIdentity, bool) {
	if gaggle != "" {
		identity := WorkflowIdentity{Gaggle: gaggle, Workflow: workflow}
		for _, candidate := range workflows {
			if candidate == identity {
				return identity, true
			}
		}
		return WorkflowIdentity{}, false
	}

	var match WorkflowIdentity
	found := false
	for _, candidate := range workflows {
		if candidate.Workflow != workflow {
			continue
		}
		if found {
			return WorkflowIdentity{}, false
		}
		match = candidate
		found = true
	}
	return match, found
}

// TriggerFiredRecord is the minimal shape ReconstructLastEval needs from a
// trigger.fired instance-journal event, kept independent of the journal
// package's Event type so this file has no import-time coupling to it.
type TriggerFiredRecord struct {
	Gaggle   string
	Workflow string
	Time     time.Time
}
