package readservice

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRunDetailProjectsAndClearsDurableChildWait(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(t, layout, machine, "parent", machine.Def.Name, "goobers", time.Now().UTC(), journal.Trigger{Kind: journal.TriggerManual}, true)
	defer func() { _ = run.Close() }()
	_, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	request := journal.ChildHandoffRequest{Gaggle: "goobers", ParentRunID: "parent", RequestID: journal.Digest([]byte("request")), Action: "wait", ChildRunID: "child", AcceptanceID: "trigger-child", InvocationKey: "inspection", SourceDigest: journal.Digest([]byte("source")), Origin: *origin}
	clock.advance(time.Second)
	marker := journal.Event{Type: journal.EventRunnerAnnotation, Stage: "implement", Attempt: 1, Runner: map[string]any{"kind": journal.ChildWaitKind, "childWait": journal.ChildWaitHeader{Version: 1, ParentRunID: "parent", Request: request}}}
	if err = run.Append(marker); err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetRun(t.Context(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	activity := detail.ChildActivity
	if activity == nil || activity.Status != "recorded" || !activity.Parked || len(activity.Waits) != 1 || activity.Waits[0].RunID != "child" || activity.Waits[0].Stage != "implement" || !activity.Waits[0].Since.Equal(clock.now) {
		t.Fatal("run detail lost durable child wait", activity)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "implement", Attempt: 1, Runner: map[string]any{"kind": journal.ChildContinuedKind, "requestId": request.RequestID}}); err != nil {
		t.Fatal(err)
	}
	detail, err = service.GetRun(t.Context(), "parent")
	if err != nil || detail.ChildActivity != nil {
		t.Fatal("continued stage still appears parked", detail.ChildActivity, err)
	}
}

func childActivityEvents(t *testing.T) []journal.Event {
	t.Helper()
	started := journal.Event{Type: journal.EventStageStarted, Stage: "inspect", Branch: 1, Attempt: 1, Seq: 2}
	attempt := journal.StageAttemptID("parent", 1, "inspect", 2)
	started.Runner = map[string]any{journal.ChildWorkflowOccurrenceKey: attempt, journal.ChildWorkflowAttemptKey: attempt}
	origin, err := journal.ChildWorkflowOriginForEvent("parent", started)
	if err != nil {
		t.Fatal(err)
	}
	request := journal.ChildHandoffRequest{Gaggle: "web", ParentRunID: "parent", RequestID: journal.Digest([]byte("request")), Action: "merge", ChildRunID: "child", AcceptanceID: "trigger-child", InvocationKey: "inspect", SourceDigest: journal.Digest([]byte("source")), Origin: *origin}
	return []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{{Branch: 1}, {Branch: 2}}},
		started,
		{Type: journal.EventRunnerAnnotation, Stage: "inspect", Branch: 1, Attempt: 1, Seq: 3, Time: time.Now(), Runner: map[string]any{"kind": journal.ChildWaitKind, "childWait": journal.ChildWaitHeader{Version: 1, ParentRunID: "parent", Request: request}}},
	}
}

func TestChildActivityDoesNotParkQueuedSiblingOrTrustMismatchedOwner(t *testing.T) {
	events := childActivityEvents(t)
	id := journal.RunIdentity{RunID: "parent", Gaggle: "web"}
	activity := recordedChildActivity(id, events)
	if activity == nil || activity.Status != "recorded" || activity.Parked || len(activity.Waits) != 1 || activity.Waits[0].Branch != 1 {
		t.Fatal("queued sibling presented as parked", activity)
	}
	for _, changed := range []journal.RunIdentity{{RunID: "other", Gaggle: "web"}, {RunID: "parent", Gaggle: "other"}} {
		got := recordedChildActivity(changed, events)
		if got == nil || got.Status != "unavailable" || got.Parent != nil || len(got.Waits) != 0 {
			t.Fatal("foreign family projected", got)
		}
	}
	events = append(events, journal.Event{Type: journal.EventBranchFinished, Branch: 1})
	if got := recordedChildActivity(id, events); got == nil || got.Status != "unavailable" {
		t.Fatal("invalid closed wait claimed authoritative", got)
	}
}

func TestChildActivityUsesValidatedRecordedParent(t *testing.T) {
	digest := journal.Digest([]byte("pin"))
	id := journal.RunIdentity{RunID: "child", Gaggle: "web", ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest,
		Child: &journal.ChildLineage{Gaggle: "web", ParentRunID: "parent", ParentWorkflow: "plan", StageOccurrence: "inspect/branch0/visit1", InvocationKey: "inspect", AcceptanceID: "trigger-child", SourceDigest: digest, EnvelopeDigest: digest}}
	got := recordedChildActivity(id, nil)
	if got == nil || got.Status != "recorded" || got.Parent == nil || got.Parent.RunID != "parent" || got.Parked {
		t.Fatal("child parent link lost", got)
	}
	id.Child.Gaggle = "other"
	if got := recordedChildActivity(id, nil); got == nil || got.Status != "unavailable" || got.Parent != nil {
		t.Fatal("invalid lineage became link", got)
	}
}
