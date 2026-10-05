package runner

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestChildWaitMarkerRemainsParkedUntilMatchingContinuation(t *testing.T) {
	_, run, frame := childOriginRuntime(t, &childOriginGoober{})
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	record := childWaitRecord{Version: 1, ParentRunID: frame.in.RunID, Request: ChildHandoffRequest{Gaggle: "web", ParentRunID: "origin-run", RequestID: journal.Digest([]byte("wait")), Action: "wait", ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "child", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}}
	event, err := childWaitEvent(frame.t.Name, 1, "", record)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "observation"}}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if !ParkedOnChild(events) {
		t.Fatal("observational event cleared child wait")
	}
	wrong := journal.Event{Type: journal.EventRunnerAnnotation, Stage: frame.t.Name, Attempt: 1, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": "wrong"}}
	if _, _, err := pendingChildWait(append(events, wrong)); err == nil {
		t.Fatal("mismatched continuation accepted")
	}
	wrong.Runner["requestId"] = record.Request.RequestID
	if ParkedOnChild(append(events, wrong)) {
		t.Fatal("matching continued event remained parked")
	}
	frameResume := &resumeFrame{segment: events}
	restored := frameResume.attemptContext(frame.in.Machine, frame.t.Name)
	if restored == nil || restored.childWait == nil || restored.attempt != 1 || restored.childWait.Request != record.Request {
		t.Fatalf("wait treated as interrupted failure: %+v", restored)
	}
}

func TestChildWaitMarkerRejectsForgedOccurrenceAndSibling(t *testing.T) {
	_, run, frame := childOriginRuntime(t, &childOriginGoober{})
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	reader, _ := journal.OpenRead(run.Dir())
	events, _ := reader.Events()
	record := childWaitRecord{Version: 1, ParentRunID: frame.in.RunID, Request: ChildHandoffRequest{Gaggle: "web", ParentRunID: "origin-run", RequestID: journal.Digest([]byte("wait")), Action: "wait", ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "child", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}}
	for _, field := range []string{"occurrence", "branch", "run"} {
		t.Run(field, func(t *testing.T) {
			changed := record
			if field == "occurrence" {
				changed.Request.Origin.StageOccurrence = "wrong"
			}
			if field == "run" {
				changed.ParentRunID = "wrong-run"
			}
			event, err := childWaitEvent(frame.t.Name, 1, "", changed)
			if err != nil {
				t.Fatal(err)
			}
			if field == "branch" {
				event.Branch = 1
			}
			if _, _, err := pendingChildWait(append(events, event)); err == nil {
				t.Fatal("forged child custody accepted")
			}
		})
	}
}
