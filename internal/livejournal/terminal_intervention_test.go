package livejournal

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func liveTerminalRequest(run string, seq, generation uint64) TerminalInterventionRequest {
	id := run + "-resume"
	fingerprint := journal.Digest([]byte(id))
	return TerminalInterventionRequest{RunID: run, Gaggle: "web", Transition: journal.TerminalIntervention{RequestID: id, Fingerprint: fingerprint, ExpectedTerminalSeq: seq, ExpectedTerminalGeneration: generation, Events: []journal.Event{
		{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "intervention.idempotency", "idempotencyKey": id, "fingerprint": fingerprint, "action": "resume", "stage": "build", "emitKey": id + "-marker"}},
		{Type: journal.EventRunResumed, Target: "build", Action: "resume", Actor: "operator", Runner: map[string]any{"emitKey": id + "-resume"}},
	}}}
}

func TestTerminalInterventionLiveWriterRehydratesResumedSegments(t *testing.T) {
	w, root := testWriter(t)
	ctx, now := t.Context(), time.Now()
	const run = "resume-live"
	if _, err := w.Emit(ctx, openBatch(run, now)); err != nil {
		t.Fatal(err)
	}
	finish := EmitRequest{RunID: run, Gaggle: "web", Ops: []Op{appendOp("first-terminal", now, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)})}}
	terminal, err := w.Emit(ctx, finish)
	if err != nil || !terminal.Terminal {
		t.Fatalf("terminal=%+v err=%v", terminal, err)
	}
	stray := EmitRequest{RunID: run, Gaggle: "web", Ops: []Op{appendOp("stray", now, journal.Event{Type: journal.EventRunResumed, Target: "build"})}}
	if _, err := w.Emit(ctx, stray); !errors.Is(err, ErrTerminal) {
		t.Fatalf("stray resume=%v", err)
	}
	req := liveTerminalRequest(run, terminal.Seq, 1)
	receipt, err := w.CommitTerminalIntervention(ctx, req)
	if err != nil || receipt.Segment != 2 || !receipt.Resumed {
		t.Fatalf("resume=%+v err=%v", receipt, err)
	}
	w.Close() // A new owner must infer the current segment, not latch the first terminal.
	reopened, err := NewWriter(func(g string) (string, bool) { return root, g == "web" })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	duplicate, err := reopened.CommitTerminalIntervention(ctx, req)
	if err != nil || !duplicate.Duplicate || duplicate.Seq != receipt.Seq {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	continued := EmitRequest{RunID: run, Gaggle: "web", Ops: []Op{appendOp("second-stage", now, journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 2}), appendOp("second-terminal", now, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})}}
	done, err := reopened.Emit(ctx, continued)
	if err != nil || !done.Terminal || done.Applied != 2 {
		t.Fatalf("continued=%+v err=%v", done, err)
	}
	events := readEvents(t, root, run)
	if len(events) != 7 {
		t.Fatalf("events=%+v", events)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatal(err)
	}
	duplicate, err = reopened.CommitTerminalIntervention(ctx, req)
	if err != nil || !duplicate.Duplicate || duplicate.Segment != 2 {
		t.Fatalf("late duplicate=%+v err=%v", duplicate, err)
	}
	if _, err := reopened.Emit(ctx, stray); !errors.Is(err, ErrTerminal) {
		t.Fatalf("post-second-terminal=%v", err)
	}
}

func TestTerminalInterventionPreservesBorrowedFileOwner(t *testing.T) {
	w, root := testWriter(t)
	const id = "borrowed-terminal"
	run := runnerHandle(t, root, id)
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	release, err := w.Adopt(id, "web", run)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	req := liveTerminalRequest(id, run.Seq(), 1)
	receipt, err := w.CommitTerminalIntervention(t.Context(), req)
	if err != nil || !receipt.Resumed || run.Closed() || run.Phase() != journal.PhaseRunning {
		t.Fatalf("receipt=%+v phase=%s closed=%v err=%v", receipt, run.Phase(), run.Closed(), err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	duplicate, err := w.CommitTerminalIntervention(t.Context(), req)
	if err != nil || !duplicate.Duplicate || run.Closed() || run.Phase() != journal.PhaseFailed {
		t.Fatalf("receipt=%+v phase=%s closed=%v err=%v", duplicate, run.Phase(), run.Closed(), err)
	}
}
