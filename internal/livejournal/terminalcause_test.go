package livejournal

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestTerminalCauseResolvesActualSequenceAndDeduplicates(t *testing.T) {
	w, runsDir := testWriter(t)
	at := time.Now()
	req := openBatch("cause-key", at)
	if _, err := w.Emit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	causalKey := "cause-key|0|build|1|1"
	cause := &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: journal.PhaseFailed, Classification: journal.TerminalInfrastructureFailure, SelectorKind: "condition", Code: "transport_failed", CausalEventSeq: 2, CausalEmitKey: causalKey}
	batch := EmitRequest{RunID: req.RunID, Gaggle: req.Gaggle, Ops: []Op{
		appendOp("progress", at, journal.Event{Type: journal.EventRunnerAnnotation}),
		appendOp(causalKey, at, journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "transport_failed"}}),
		appendOp("terminal", at, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed), TerminalCause: cause}),
	}}
	for i := 0; i < 2; i++ {
		if _, err := w.Emit(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	events := readEvents(t, runsDir, req.RunID)
	got := events[len(events)-1].TerminalCause
	if got == nil || got.CausalEventSeq != 4 || len(events) != 5 {
		t.Fatalf("cause=%+v events=%d", got, len(events))
	}
	if cause.CausalEventSeq != 2 {
		t.Fatal("writer mutated caller's deterministic projection")
	}
}
