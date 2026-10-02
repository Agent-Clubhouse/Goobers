package localscheduler

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// triggerStateRecorder captures every trigger-state snapshot a scheduler
// tries to persist, plus how many runs had started when each write happened.
type triggerStateRecorder struct {
	mu              sync.Mutex
	snapshots       []map[WorkflowIdentity]time.Time
	startsAtWrite   []int
	err             error
	startsSoFarFunc func() int
}

func (r *triggerStateRecorder) write(_ string, evaluations map[WorkflowIdentity]time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots = append(r.snapshots, maps.Clone(evaluations))
	r.startsAtWrite = append(r.startsAtWrite, r.startsSoFarFunc())
	return r.err
}

func (r *triggerStateRecorder) calls() ([]map[WorkflowIdentity]time.Time, []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[WorkflowIdentity]time.Time(nil), r.snapshots...), append([]int(nil), r.startsAtWrite...)
}

// batchTestEntries declares three hourly workflows and one daily workflow
// that is never due in these tests, each in its own gaggle.
func batchTestEntries(starter *fakeStarter) []WorkflowEntry {
	return []WorkflowEntry{
		{Gaggle: "alpha", Workflow: "alpha", Schedules: []Schedule{fakeSchedule{d: time.Hour}}, Starter: starter},
		{Gaggle: "beta", Workflow: "beta", Schedules: []Schedule{fakeSchedule{d: time.Hour}}, Starter: starter},
		{Gaggle: "gamma", Workflow: "gamma", Schedules: []Schedule{fakeSchedule{d: time.Hour}}, Starter: starter},
		{Gaggle: "idle", Workflow: "idle", Schedules: []Schedule{fakeSchedule{d: 24 * time.Hour}}, Starter: starter},
	}
}

func batchIdentity(name string) WorkflowIdentity {
	return WorkflowIdentity{Gaggle: name, Workflow: name}
}

// TestTickPersistsTriggerStateOncePerTick pins #6010: a tick with several due
// workflows writes the trigger-state snapshot exactly once, the snapshot holds
// every advanced LastEval plus the unchanged ones, and the write lands before
// any due workflow is dispatched.
func TestTickPersistsTriggerStateOncePerTick(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	scheduler, _ := newTestScheduler(t, batchTestEntries(starter))
	recorder := &triggerStateRecorder{startsSoFarFunc: starter.count}
	scheduler.writeTriggerState = recorder.write

	scheduler.mu.Lock()
	idleLastEval := scheduler.triggers[batchIdentity("idle")].LastEval
	base := scheduler.triggers[batchIdentity("alpha")].LastEval
	scheduler.mu.Unlock()
	// New stamps each trigger's LastEval separately, so step a second past
	// the hour to make every hourly workflow due.
	now := base.Add(time.Hour + time.Second)

	scheduler.Tick(context.Background(), now)
	waitForCount(t, starter.count, 3)
	scheduler.Wait()

	snapshots, startsAtWrite := recorder.calls()
	if len(snapshots) != 1 {
		t.Fatalf("trigger-state writes = %d, want exactly one for the tick", len(snapshots))
	}
	if startsAtWrite[0] != 0 {
		t.Fatalf("runs started before the trigger-state write = %d, want 0", startsAtWrite[0])
	}
	for _, workflow := range []string{"alpha", "beta", "gamma"} {
		if got := snapshots[0][batchIdentity(workflow)]; !got.Equal(now) {
			t.Fatalf("%s LastEval = %s, want advanced to %s", workflow, got, now)
		}
	}
	if got := snapshots[0][batchIdentity("idle")]; !got.Equal(idleLastEval) {
		t.Fatalf("idle LastEval = %s, want unchanged %s", got, idleLastEval)
	}

	// A tick in which nothing advances writes nothing.
	scheduler.Tick(context.Background(), now)
	if snapshots, _ := recorder.calls(); len(snapshots) != 1 {
		t.Fatalf("trigger-state writes after a no-op tick = %d, want still 1", len(snapshots))
	}
}

// TestTickPersistFailureJournalsEachEvaluatedWorkflowAndStillDispatches keeps
// the pre-batching failure contract: one trigger_state_persist_failed error per
// workflow whose LastEval advanced, and every due workflow still dispatches.
func TestTickPersistFailureJournalsEachEvaluatedWorkflowAndStillDispatches(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	scheduler, dir := newTestScheduler(t, batchTestEntries(starter))
	recorder := &triggerStateRecorder{startsSoFarFunc: starter.count, err: errors.New("state unavailable")}
	scheduler.writeTriggerState = recorder.write

	scheduler.mu.Lock()
	base := scheduler.triggers[batchIdentity("alpha")].LastEval
	scheduler.mu.Unlock()
	scheduler.Tick(context.Background(), base.Add(time.Hour+time.Second))
	waitForCount(t, starter.count, 3)
	scheduler.Wait()

	if snapshots, _ := recorder.calls(); len(snapshots) != 1 {
		t.Fatalf("trigger-state writes = %d, want one attempt for the tick", len(snapshots))
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	failed := map[string]int{}
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "trigger_state_persist_failed" {
			failed[event.Workflow]++
		}
	}
	want := map[string]int{"alpha": 1, "beta": 1, "gamma": 1}
	if !maps.Equal(failed, want) {
		t.Fatalf("persist-failure events by workflow = %v, want %v", failed, want)
	}
}

// TestTickBatchedTriggerStateSurvivesCrashMidTick proves the crash-durability
// side of #6010. The batched snapshot is written before any dispatch, so a
// crash part-way through dispatch leaves a file that claims every due workflow
// advanced even though only some fired. Restart must neither re-fire the
// workflow that fired nor lose the one that did not: recovery rebuilds
// LastEval from the journal's trigger.fired history, not from that snapshot.
func TestTickBatchedTriggerStateSurvivesCrashMidTick(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	tickAt := base.Add(time.Hour)
	clock := base
	past, _, err := journal.OpenInstanceLog(dir, journal.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	// Both workflows fired on the previous boundary; on this tick only alpha's
	// dispatch was journaled before the process died.
	for _, workflow := range []string{"alpha", "beta"} {
		if err := past.Append(journal.Event{Type: journal.EventTriggerFired, Workflow: workflow, Reason: "scheduled"}); err != nil {
			t.Fatal(err)
		}
	}
	clock = tickAt
	if err := past.Append(journal.Event{Type: journal.EventTriggerFired, Workflow: "alpha", Reason: "scheduled"}); err != nil {
		t.Fatal(err)
	}
	if err := past.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeTriggerEvaluations(dir, newStateOwner(), map[WorkflowIdentity]time.Time{
		{Workflow: "alpha"}: tickAt,
		{Workflow: "beta"}:  tickAt,
	}); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	alpha := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	scheduler := New([]WorkflowEntry{
		{Workflow: "alpha", Schedules: []Schedule{fakeSchedule{d: time.Hour}}, Starter: alpha},
		{Workflow: "beta", Schedules: []Schedule{fakeSchedule{d: time.Hour}}, Starter: beta},
	}, log)
	restartAt := tickAt.Add(time.Minute)
	if err := scheduler.ReconcileAll(nil, restartAt); err != nil {
		t.Fatal(err)
	}

	scheduler.Tick(context.Background(), restartAt)
	waitForCount(t, beta.count, 1)
	scheduler.Wait()
	if got := alpha.count(); got != 0 {
		t.Fatalf("alpha starts after restart = %d, want 0 (already fired before the crash)", got)
	}
	if got := beta.count(); got != 1 {
		t.Fatalf("beta starts after restart = %d, want exactly 1 (its firing must not be lost)", got)
	}
}
