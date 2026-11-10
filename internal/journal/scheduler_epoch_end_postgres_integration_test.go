//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type memorySchedulerState struct{}

func (memorySchedulerState) ReadScheduleDemand(string) (map[localscheduler.WorkflowIdentity]bool, error) {
	return map[localscheduler.WorkflowIdentity]bool{}, nil
}
func (memorySchedulerState) ReadTriggerEvaluations(string) (map[localscheduler.WorkflowIdentity]time.Time, error) {
	return map[localscheduler.WorkflowIdentity]time.Time{}, nil
}
func (memorySchedulerState) WriteScheduleDemand(string, map[localscheduler.WorkflowIdentity]bool) error {
	return nil
}
func (memorySchedulerState) WriteTriggerEvaluations(string, map[localscheduler.WorkflowIdentity]time.Time) error {
	return nil
}

type epochEngine struct{ started chan string }

func (e epochEngine) Start(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
	e.started <- in.RunID
	return engine.StartResult{RunID: in.RunID}, nil
}

type placeholderStarter struct{}

func (placeholderStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	return localscheduler.StartResult{}, errors.New("placeholder starter must be replaced")
}

// A cloud Starter that unwinds at epoch end makes the scheduler try to journal
// "run.finished: error: context canceled" for a run that is STILL LIVE in
// Temporal. The real cloud instance log must refuse that record: it is bound to
// the epoch context and refuses appends once it is done, so the record is never
// written (a successor would otherwise read a false terminal).
func TestEpochEndWritesNoRunFinishedForALiveRun(t *testing.T) {
	backend := postgresBackend(t, postgresJournalConfig(t))
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	var refused atomic.Int32
	log, err := backend.OpenSchedulerLog(epoch, func(error) { refused.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	const runID = "0123456789abcdef0123456789abcdef"
	var polls atomic.Int32
	config := schedstart.Config{
		Engine: epochEngine{started: make(chan string, 1)},
		Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			return engine.RunInput{RunID: runID}, nil
		},
		Terminal: func(ctx context.Context, _ string) (schedstart.Outcome, error) {
			if polls.Add(1) == 1 {
				return schedstart.Outcome{}, errors.New("temporal unavailable")
			}
			<-ctx.Done()
			return schedstart.Outcome{}, ctx.Err()
		},
		TerminalBackoff: schedstart.Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond},
	}
	var tracker schedstart.Tracker
	entry := localscheduler.WorkflowEntry{Workflow: "repair", Gaggle: "own", Starter: placeholderStarter{}}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSchedulerState(memorySchedulerState{}))
	entry.Starter = config.StarterFor(triggerqueue.Record{ID: "trigger-" + runID}, &tracker)
	if _, err := scheduler.TriggerPreparedOrdinary(epoch, epoch, entry, runID, localscheduler.PreparedTriggerOptions{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	<-config.Engine.(epochEngine).started
	for polls.Load() < 2 { // past the transient error, now blocked on the live run
		time.Sleep(5 * time.Millisecond)
	}

	cancel() // epoch ends; the run is still live
	scheduler.Wait()
	waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := tracker.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}

	events, err := backend.InstanceRead(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawFired bool
	for _, event := range events {
		switch event.Type {
		case journal.EventTriggerFired:
			sawFired = true
		case journal.EventRunFinished:
			t.Fatalf("a run.finished was journaled at epoch end for a live run: %+v", event)
		}
	}
	if !sawFired {
		t.Fatalf("premise: the dispatch's trigger.fired should be journaled, got %d events", len(events))
	}
	if refused.Load() == 0 {
		t.Fatal("premise: the scheduler's run.finished append was never attempted (and refused)")
	}
}
