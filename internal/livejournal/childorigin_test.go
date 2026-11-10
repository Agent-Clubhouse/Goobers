package livejournal

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestChildOriginReceiptSurvivesDedupAndRehydration(t *testing.T) {
	w, dir := testWriter(t)
	batch := openBatch("run-child-origin", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	batch.Ops[1].ChildStageStart = &ChildStageStart{}
	batch.Ops = append(batch.Ops, appendOp("after-start", batch.Ops[1].Time, journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "after-start"}}))
	first, err := w.Emit(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ChildOrigins) != 1 || first.ChildOrigins[0].Seq >= first.Seq {
		t.Fatalf("receipt used batch watermark: %+v", first)
	}
	events := readEvents(t, dir, batch.RunID)
	origin, err := journal.ChildWorkflowOriginForEvent(batch.RunID, events[1])
	if err != nil || first.ChildOrigins[0].Origin != *origin || first.ChildOrigins[0].Seq != events[1].Seq {
		t.Fatalf("receipt not bound to actual event: %+v %v", first, err)
	}
	for _, reopen := range []bool{false, true} {
		if reopen {
			w.CloseIdle(0)
		}
		retried, err := w.Emit(context.Background(), batch)
		if err != nil || !reflect.DeepEqual(retried.ChildOrigins, first.ChildOrigins) || retried.Applied != 0 {
			t.Fatalf("dedup (reopen=%v): %+v %v", reopen, retried, err)
		}
	}
	continuation := appendOp("continuation", batch.Ops[1].Time, journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 2, AttemptClass: journal.AttemptInfra})
	continuation.ChildStageStart = &ChildStageStart{Continuation: true}
	continued, err := w.Emit(context.Background(), EmitRequest{RunID: batch.RunID, Gaggle: batch.Gaggle, Ops: []Op{continuation}})
	if err != nil {
		t.Fatal(err)
	}
	if continued.ChildOrigins[0].Class != journal.AttemptInfra || continued.ChildOrigins[0].Origin.StageOccurrence != origin.StageOccurrence || continued.ChildOrigins[0].Origin.AttemptID == origin.AttemptID {
		t.Fatal("retry did not retain occurrence with a new committed attempt")
	}
	revisit := appendOp("revisit", batch.Ops[1].Time, journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1})
	revisit.ChildStageStart = &ChildStageStart{}
	visited, err := w.Emit(context.Background(), EmitRequest{RunID: batch.RunID, Gaggle: batch.Gaggle, Ops: []Op{revisit}})
	if err != nil || visited.ChildOrigins[0].Origin.StageOccurrence == origin.StageOccurrence {
		t.Fatalf("revisit reused occurrence: %+v %v", visited, err)
	}
	// Same emit key cannot silently change its requested start or continuation.
	continuation.ChildStageStart.Continuation = false
	if _, err := w.Emit(context.Background(), EmitRequest{RunID: batch.RunID, Gaggle: batch.Gaggle, Ops: []Op{continuation}}); err == nil {
		t.Fatal("changed continuation accepted on duplicate key")
	}
}

func TestChildOriginCannotBeInventedFromLegacyWatermark(t *testing.T) {
	w, _ := testWriter(t)
	batch := openBatch("run-legacy-origin", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	response, err := w.Emit(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(response)
	if err != nil || string(encoded) != `{"applied":1,"deduplicated":1,"seq":2}` {
		t.Fatalf("legacy acknowledgement changed: %s %v", encoded, err)
	}
	batch.Ops[1].ChildStageStart = &ChildStageStart{}
	if _, err := w.Emit(context.Background(), batch); err == nil {
		t.Fatal("historical unbound event acquired child authority")
	}
}

func TestChildOriginBindingRejectsForgedMetadataBeforeAppend(t *testing.T) {
	w, _ := testWriter(t)
	for _, key := range []string{journal.ChildWorkflowOccurrenceKey, journal.ChildWorkflowAttemptKey} {
		batch := openBatch("run-forged-origin", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
		batch.Ops[1].ChildStageStart = &ChildStageStart{}
		batch.Ops[1].Event.Runner = map[string]any{key: "forged"}
		if _, err := w.Emit(context.Background(), batch); err == nil {
			t.Fatal("caller-supplied child identity accepted")
		}
		batch.Ops[1].ChildStageStart = nil
		if _, err := w.Emit(context.Background(), batch); err == nil {
			t.Fatal("ordinary append forged writer-owned child identity")
		}
	}
	batch := openBatch("run-forged-origin", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	batch.Ops[0].ChildStageStart = &ChildStageStart{}
	if _, err := w.Emit(context.Background(), batch); err == nil {
		t.Fatal("binding accepted on run.started")
	}
}
