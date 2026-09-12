package localscheduler

import (
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// The scheduler and engine starter both echo run.started into the instance
// journal. Restarting must restore admissions, rather than count both echoes.
func TestReconcileBudgetCountsDistinctAdmissions(t *testing.T) {
	now := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)
	alpha := WorkflowIdentity{Gaggle: "alpha", Workflow: "deploy"}
	beta := WorkflowIdentity{Gaggle: "beta", Workflow: "deploy"}
	other := WorkflowIdentity{Gaggle: "alpha", Workflow: "verify"}
	start := func(id WorkflowIdentity, runID string) journal.Event {
		return journal.Event{Type: journal.EventRunStarted, Gaggle: id.Gaggle, Workflow: id.Workflow, RunID: runID}
	}
	for _, tc := range []struct {
		name      string
		events    []journal.Event
		remaining map[WorkflowIdentity]int
		firstAge  time.Duration
		reset     bool
		maxPerDay int32
	}{
		{"scheduler and engine echoes", []journal.Event{start(alpha, "run-a"), start(alpha, "run-a")}, map[WorkflowIdentity]int{alpha: 1}, 10 * time.Minute, false, 0},
		{"distinct runs still consume budget", []journal.Event{start(alpha, "run-a"), start(alpha, "run-b")}, map[WorkflowIdentity]int{alpha: 0}, 10 * time.Minute, false, 0},
		{"anonymous legacy starts cannot be deduplicated", []journal.Event{start(alpha, ""), start(alpha, "")}, map[WorkflowIdentity]int{alpha: 0}, 10 * time.Minute, false, 0},
		{"same run spelling in different scopes stays distinct", []journal.Event{start(alpha, "run-a"), start(beta, "run-a"), start(other, "run-a"), start(alpha, "run-a")}, map[WorkflowIdentity]int{alpha: 1, beta: 1, other: 1}, 10 * time.Minute, false, 0},
		{"echo does not extend hourly window", []journal.Event{start(alpha, "run-a"), start(alpha, "run-a")}, map[WorkflowIdentity]int{alpha: 2}, time.Hour + 5*time.Millisecond, false, 0},
		{"echo does not reenter daily cutoff", []journal.Event{start(alpha, "run-a"), start(alpha, "run-a")}, map[WorkflowIdentity]int{alpha: 2}, 24*time.Hour + 5*time.Millisecond, false, 2},
		{"echo does not undo rate reset", []journal.Event{start(alpha, "run-a"), start(alpha, "run-a")}, map[WorkflowIdentity]int{alpha: 2}, 10 * time.Minute, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "scheduler")
			at := now.Add(-tc.firstAge)
			if tc.reset {
				if err := WriteRateReset(dir, at.Add(5*time.Millisecond)); err != nil {
					t.Fatal(err)
				}
			}
			log, _, err := journal.OpenInstanceLog(dir, journal.WithClock(func() time.Time { return at }))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := log.Close(); err != nil {
					t.Error(err)
				}
			}()
			for i, event := range tc.events {
				if i > 0 {
					event.Reason = "engine dispatch admitted"
				}
				if err := log.Append(event); err != nil {
					t.Fatal(err)
				}
				at = at.Add(10 * time.Millisecond)
			}
			readiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 20, MaxRunsPerHour: 2, MaxRunsPerDay: tc.maxPerDay}
			entries := []WorkflowEntry{}
			for id := range tc.remaining {
				entries = append(entries, WorkflowEntry{Gaggle: id.Gaggle, Workflow: id.Workflow, Readiness: readiness})
			}
			sched := New(entries, log)
			if err := sched.Reconcile(filepath.Join(t.TempDir(), "runs"), now); err != nil {
				t.Fatal(err)
			}
			for id, remaining := range tc.remaining {
				for i := 0; i < remaining; i++ {
					if ok, reason := sched.conditions.AdmitWorkflow(id, readiness, now); !ok {
						t.Fatalf("%+v admission %d refused after restart: %s", id, i+1, reason)
					}
				}
				if ok, reason := sched.conditions.AdmitWorkflow(id, readiness, now); ok || reason != ReasonBudget {
					t.Fatalf("%+v exhausted budget: admitted=%v reason=%q", id, ok, reason)
				}
			}
		})
	}
}
