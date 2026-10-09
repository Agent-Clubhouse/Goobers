package main

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestChildJournalRefusesHostAuthorityAndOtherAttempts(t *testing.T) {
	c := childpod.Contract{Identity: journal.RunIdentity{RunID: strings.Repeat("a", 32), Gaggle: "g"}, Stage: "work", Attempt: 2}
	base := livejournal.EmitRequest{RunID: c.Identity.RunID, Gaggle: "g"}
	for _, kind := range []journal.EventType{journal.EventRunStarted, journal.EventRunFinished, journal.EventRunResumed, journal.EventStageStarted, journal.EventStageFinished, journal.EventReviewerStarted, journal.EventReviewerFinished, journal.EventGateEvaluated, journal.EventInputSnapshot, journal.EventArtifactRecorded, journal.EventOperatorMessageRequested} {
		base.Ops = []livejournal.Op{{Kind: livejournal.OpAppend, Key: "key", Event: &journal.Event{Type: kind, Stage: "work", Attempt: 2}}}
		if _, err := childJournalRequest(c, journal.Digest(nil), base); err == nil {
			t.Fatalf("worker authored %s", kind)
		}
	}
	for _, kind := range []string{childPodWriterStarted, childPodWriterJoined, "run.recovery", "child.workspace.writer.joined"} {
		base.Ops = []livejournal.Op{{Kind: livejournal.OpAppend, Key: "key", Event: &journal.Event{Type: journal.EventRunnerAnnotation, Stage: "work", Attempt: 2, Runner: map[string]any{"kind": kind}}}}
		if _, err := childJournalRequest(c, journal.Digest(nil), base); err == nil {
			t.Fatalf("worker forged custody marker %s", kind)
		}
	}
	for _, op := range []livejournal.Op{
		{Kind: livejournal.OpArtifact, Artifact: &livejournal.ArtifactOp{Stage: "work", Attempt: 2, Name: "trusted", Integrity: apiv1.IntegrityTrusted}},
		{Kind: livejournal.OpArtifact, Artifact: &livejournal.ArtifactOp{Stage: "work", Attempt: 2, Ref: &journal.Ref{Integrity: apiv1.IntegrityTrusted}}},
		{Kind: livejournal.OpSpan, Span: &livejournal.SpanOp{Stage: "work", Attempt: 2, Ref: journal.Ref{Integrity: apiv1.IntegrityTrusted}}},
		{Kind: livejournal.OpAppend, Event: &journal.Event{Type: journal.EventAgentMessage, Stage: "other", Attempt: 2}},
		{Kind: livejournal.OpAppend, Event: &journal.Event{Type: journal.EventAgentMessage, Stage: "work", Attempt: 1}},
		{Kind: livejournal.OpAppend, Event: &journal.Event{Type: journal.EventAgentMessage, Stage: "foreign:work", Attempt: 2}},
		{Kind: livejournal.OpAppend, Event: &journal.Event{Type: journal.EventAgentMessage, Stage: "work", Attempt: 2, Branch: 1}},
		{Kind: livejournal.OpInstanceAnnotation, Event: &journal.Event{Type: journal.EventAgentMessage, Stage: "work", Attempt: 2}},
		{Kind: livejournal.OpArtifact, Artifact: &livejournal.ArtifactOp{Stage: "work", Attempt: 2}, Event: &journal.Event{Type: journal.EventRunFinished}},
	} {
		op.Key = "key"
		base.Ops = []livejournal.Op{op}
		if _, err := childJournalRequest(c, journal.Digest(nil), base); err == nil {
			t.Fatalf("untrusted operation accepted: %+v", op)
		}
	}
}

func TestChildJournalNamespacesObservationsAndProjectsPayload(t *testing.T) {
	c := childpod.Contract{Identity: journal.RunIdentity{RunID: strings.Repeat("a", 32), Gaggle: "g"}, Stage: "work", Attempt: 2}
	event := &journal.Event{Type: journal.EventAgentMessage, Stage: c.Identity.RunID + ":work", Actor: "forged-human", Status: "completed", Ref: &journal.Ref{Integrity: apiv1.IntegrityTrusted}}
	req := livejournal.EmitRequest{RunID: c.Identity.RunID, Gaggle: "g", Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "message/1", Event: event}}}
	a, err := childJournalRequest(c, journal.Digest([]byte("physical-1")), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := childJournalRequest(c, journal.Digest([]byte("physical-2")), req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Ops[0].Key == b.Ops[0].Key || a.Ops[0].Event.Stage != "work" || a.Ops[0].Event.Attempt != 2 || a.Ops[0].Event.Ref != nil || a.Ops[0].Event.Actor != "" || a.Ops[0].Event.Status != "" {
		t.Fatalf("attempt scope or payload projection failed: %+v", a.Ops[0])
	}
	if event.Actor == "" || req.Ops[0].Key != "message/1" || event.Attempt != 0 {
		t.Fatal("mutated caller request")
	}
	again, err := childJournalRequest(c, journal.Digest([]byte("physical-1")), req)
	if err != nil || !reflect.DeepEqual(a, again) {
		t.Fatal("replayed observation changed", err)
	}
	for _, change := range []func(*livejournal.EmitRequest){
		func(r *livejournal.EmitRequest) { r.RunID = "other" },
		func(r *livejournal.EmitRequest) { r.Gaggle = "other" },
		func(r *livejournal.EmitRequest) { r.Open = &livejournal.OpenHeader{} },
	} {
		foreign := req
		change(&foreign)
		if _, err := childJournalRequest(c, journal.Digest(nil), foreign); err == nil {
			t.Fatal("foreign journal ownership accepted")
		}
	}
}

func TestChildJournalNamespacesTranscriptCaptureWithoutChangingSequence(t *testing.T) {
	c := childpod.Contract{Identity: journal.RunIdentity{RunID: "run", Gaggle: "g"}, Stage: "work", Attempt: 1}
	capture := strings.Repeat("b", 32)
	req := livejournal.EmitRequest{RunID: "run", Gaggle: "g", Ops: []livejournal.Op{{Kind: livejournal.OpTranscriptCheckpoint, Key: capture + "/checkpoint/4", Checkpoint: &livejournal.TranscriptCheckpointOp{Capture: capture, Stage: "run:work", Action: "append"}}}}
	out, err := childJournalRequest(c, journal.Digest(nil), req)
	if err != nil {
		t.Fatal(err)
	}
	op := out.Ops[0]
	if op.Checkpoint.Capture == capture || op.Key != op.Checkpoint.Capture+"/checkpoint/4" || op.Checkpoint.Stage != "work" {
		t.Fatal("capture scope changed sequence", op)
	}
}
