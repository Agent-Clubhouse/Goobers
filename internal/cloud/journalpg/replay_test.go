package journalpg

import (
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestIncrementalReplayMatchesFullReplayAtEveryPrefix(t *testing.T) {
	types := []journal.EventType{journal.EventRunStarted, journal.EventRunFinished, journal.EventRunResumed, journal.EventStageRerunRequested, journal.EventGateStarted, journal.EventGatePaused, journal.EventGateEvaluated, journal.EventGateOverridden, journal.EventParallelStarted, journal.EventParallelFinished, journal.EventMapStarted, journal.EventMapFinished, journal.EventBranchStarted, journal.EventBranchFinished, journal.EventStageStarted, journal.EventRunnerAnnotation}
	names := []string{"", "a", "b", "c"}
	targets := []string{"next", journal.TargetAbort, journal.TargetEscalate, "@join"}
	statuses := []string{"completed", "failed", "aborted", "escalated", ""}
	rng := rand.New(rand.NewPCG(7853, 1))
	replay := newReplayState("run", nil)
	var events []journal.Event
	for i := range 1500 {
		event := journal.Event{Seq: uint64(i + 1), Time: time.Unix(int64(i), 0).UTC(), Type: types[rng.IntN(len(types))], Branch: rng.IntN(4), Gate: names[rng.IntN(len(names))], Stage: names[rng.IntN(len(names))], Parallel: names[rng.IntN(len(names))], BranchName: "branch", Target: targets[rng.IntN(len(targets))], Status: statuses[rng.IntN(len(statuses))]}
		if rng.IntN(2) == 0 {
			event.BranchStatus = journal.BranchSucceeded
		}
		if rng.IntN(2) == 0 {
			event.Error = &journal.ErrorDetail{Message: "reason"}
		}
		events = append(events, event)
		replay.fold([]journal.Event{event})
		checkpoint, err := replay.checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		want := stateForEvents("run", events)
		if !reflect.DeepEqual(checkpoint.state, want) {
			t.Fatalf("prefix %d event=%+v\n got=%+v\nwant=%+v", i+1, event, checkpoint.state, want)
		}
	}
}

func TestIncrementalReplayDoesNotApplyDeduplicatedEventsAgain(t *testing.T) {
	events := []journal.Event{{Seq: 1, Type: journal.EventRunFinished, Status: "failed"}, {Seq: 2, Type: journal.EventRunResumed, Target: "retry"}}
	replay := newReplayState("run", events)
	replay.fold(events[:1])
	checkpoint, err := replay.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if want := stateForEvents("run", events); !reflect.DeepEqual(checkpoint.state, want) {
		t.Fatalf("got=%+v want=%+v", checkpoint.state, want)
	}
}
