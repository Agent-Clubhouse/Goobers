package journal

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func parallelWaitFixture() ([]Event, Event, Event) {
	events := []Event{{Type: EventParallelStarted, Parallel: "fan", Completeness: []BranchOutcome{{Branch: 1}, {Branch: 2}}}}
	start := func(branch int, seq uint64) Event {
		stage := fmt.Sprintf("stage-%d", branch)
		attempt := StageAttemptID("parent", branch, stage, seq)
		return Event{Type: EventStageStarted, Seq: seq, Branch: branch, Stage: stage, Attempt: 1, Runner: map[string]any{ChildWorkflowOccurrenceKey: attempt, ChildWorkflowAttemptKey: attempt}}
	}
	wait := func(started Event) Event {
		origin, _ := ChildWorkflowOriginForEvent("parent", started)
		child := fmt.Sprintf("child-%d", started.Branch)
		request := ChildHandoffRequest{Gaggle: "web", ParentRunID: "parent", RequestID: Digest([]byte(child)), Action: "wait", ChildRunID: child, AcceptanceID: "trigger-" + child, InvocationKey: child, SourceDigest: Digest([]byte("source")), Origin: *origin}
		return Event{Type: EventRunnerAnnotation, Branch: started.Branch, Stage: started.Stage, Attempt: 1, Runner: map[string]any{"kind": ChildWaitKind, "childWait": ChildWaitHeader{Version: 1, ParentRunID: "parent", Request: request}}}
	}
	a, b := start(1, 2), start(2, 4)
	wa, wb := wait(a), wait(b)
	events = append(events, a, wa, b, wb)
	for i := range events {
		events[i].Seq = uint64(i + 1)
		events[i].Time = time.Unix(1000+int64(i)*60, 0)
	}
	return events, wa, wb
}

func TestChildWaitProjectionAccountsForQueuedRunningAndFinishedSiblings(t *testing.T) {
	events, wa, wb := parallelWaitFixture()
	for _, test := range []struct {
		name     string
		events   []Event
		parked   bool
		runnable []int
	}{
		{"queued sibling", events[:3], false, []int{2}},
		{"running sibling", events[:4], false, []int{2}},
		{"both waiting", events, true, nil},
		{"sibling stage finished", append(append([]Event{}, events[:3]...), Event{Type: EventStageFinished, Branch: 2, Stage: wb.Stage, Attempt: 1}), false, []int{2}},
		{"sibling branch finished", append(append([]Event{}, events[:3]...), Event{Type: EventBranchFinished, Branch: 2, Parallel: "fan"}), true, nil},
		{"human reopens one branch", append(append([]Event{}, events...), Event{Type: EventRunFinished}, Event{Type: EventRunResumed}, wa), false, []int{2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := ProjectChildWaits(test.events)
			if err != nil || p.Parked() != test.parked || !reflect.DeepEqual(p.RunnableBranches, test.runnable) {
				t.Fatalf("projection %+v, %v", p, err)
			}
			wait, found := p.Waits[1]
			if !found || wait.Marker.Stage != wa.Stage {
				t.Fatal("first branch custody missing", wait)
			}
		})
	}
	if _, _, err := PendingChildWait(events); err == nil {
		t.Fatal("ambiguous branch accepted")
	}
}

func TestChildWaitProjectionRefusesCrossBranchContinuationAndReplacement(t *testing.T) {
	events, wa, wb := parallelWaitFixture()
	a := wa.Runner["childWait"].(ChildWaitHeader)
	b := wb.Runner["childWait"].(ChildWaitHeader)
	for _, next := range []Event{
		{Type: EventRunnerAnnotation, Branch: 1, Stage: wa.Stage, Attempt: 1, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": b.Request.RequestID}},
		{Type: EventStageStarted, Branch: 1, Stage: "new", Attempt: 2},
		{Type: EventBranchFinished, Branch: 1, Parallel: "fan"},
		{Type: EventParallelFinished, Parallel: "fan"},
	} {
		if _, err := ProjectChildWaits(append(append([]Event{}, events...), next)); err == nil {
			t.Fatal("unresolved branch custody accepted", next.Type)
		}
	}
	continued := Event{Type: EventRunnerAnnotation, Branch: 1, Stage: wa.Stage, Attempt: 1, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": a.Request.RequestID}}
	p, err := ProjectChildWaits(append(events, continued))
	if err != nil || p.Parked() || len(p.Waits) != 1 || p.Waits[2].Header.Request != b.Request {
		t.Fatal(p, err)
	}
	bad := wa
	bad.Branch = 2
	if _, err := DecodeChildWaitHeader(bad, events[1]); err == nil {
		t.Fatal("sibling origin accepted")
	}
}

func TestChildWaitClocksCountOnlyWholeRunParking(t *testing.T) {
	events, wa, _ := parallelWaitFixture()
	header := wa.Runner["childWait"].(ChildWaitHeader)
	start, now := events[0].Time, events[0].Time.Add(10*time.Minute)
	continued := Event{Type: EventRunnerAnnotation, Time: start.Add(8 * time.Minute), Branch: 1, Stage: wa.Stage, Attempt: 1, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": header.Request.RequestID}}
	events = append(events, continued)
	whole, err := ChildExecutionElapsed(events, start, now, nil)
	if err != nil || whole != 6*time.Minute {
		t.Fatal(whole, err)
	}
	branch := 1
	elapsed, err := ChildExecutionElapsed(events, start, now, &branch)
	if err != nil || elapsed != 4*time.Minute {
		t.Fatal(elapsed, err)
	}
	branch = 2
	elapsed, err = ChildExecutionElapsed(events, start, now, &branch)
	if err != nil || elapsed != 4*time.Minute {
		t.Fatal(elapsed, err)
	}
}

func TestOrdinaryParallelJournalsDoNotGainChildValidation(t *testing.T) {
	events := []Event{{Type: EventParallelStarted, Parallel: "old"}, {Type: EventBranchStarted, Branch: 1}, {Type: EventStageStarted, Branch: 1, Stage: "a", Status: string(apiv1.ResultSuccess)}}
	p, err := ProjectChildWaits(events)
	if err != nil || p.Parked() {
		t.Fatal(p, err)
	}
}

func TestChildWaitProjectionRefusesInvalidDeclaredOwners(t *testing.T) {
	for _, owners := range [][]BranchOutcome{nil, {{Branch: 0}}, {{Branch: 129}}, {{Branch: 1}, {Branch: 1}}} {
		events, _, _ := parallelWaitFixture()
		events[0].Completeness = owners
		if _, err := ProjectChildWaits(events); err == nil {
			t.Fatalf("invalid declared owners accepted: %+v", owners)
		}
	}
}

// This private preparation already supports bound branch execution internally;
// public workflow starts and parallel child queue admission remain gated.
func TestChildWaitProjectionAgreesWithPreparedParallelRuntime(t *testing.T) {
	events, wait, _ := parallelWaitFixture()
	projected, err := ProjectChildWaits(events[:3])
	if err != nil || len(projected.Waits) != 1 {
		t.Fatal(projected, err)
	}
	header, err := DecodeChildWaitHeader(wait, events[1])
	if err != nil || header.Request != projected.Waits[wait.Branch].Header.Request {
		t.Fatal("projection and prepared branch custody differ", err)
	}
}
