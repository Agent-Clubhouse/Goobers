package livejournal

import (
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestStartAcknowledgmentInterleavingDedupAndRestart(t *testing.T) {
	w, runsDir := testWriter(t)
	at := time.Now()
	open := openBatch("start-ack", at)
	if _, err := w.Emit(t.Context(), open); err != nil {
		t.Fatal(err)
	}
	start := appendOp("review-start", at, journal.ReviewerAttemptEvent(journal.EventReviewerStarted, "review", 2, journal.AttemptInfra))
	batch := EmitRequest{RunID: open.RunID, Gaggle: open.Gaggle, Ops: []Op{appendOp("aux-before", at, journal.Event{Type: journal.EventRunnerAnnotation}), start, appendOp("aux-after", at, journal.Event{Type: journal.EventRunnerAnnotation})}}
	first, err := w.Emit(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Starts) != 1 || first.Starts[0].Seq != 4 || first.Seq != 5 {
		t.Fatalf("ack=%+v", first)
	}
	w.Close()
	w, err = NewWriter(func(string) (string, bool) { return runsDir, true })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Emit(t.Context(), EmitRequest{RunID: open.RunID, Gaggle: open.Gaggle, Ops: []Op{appendOp("later-aux", at, journal.Event{Type: journal.EventRunnerAnnotation})}}); err != nil {
		t.Fatal(err)
	}
	again, err := w.Emit(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Starts, again.Starts) || again.Seq != 6 {
		t.Fatalf("dedup ack changed: %+v %+v", first, again)
	}
	// Reusing another event's key cannot relabel its accepted anchor.
	forged := start
	event := *start.Event
	event.Stage = "different"
	forged.Event = &event
	got, err := w.Emit(t.Context(), EmitRequest{RunID: open.RunID, Gaggle: open.Gaggle, Ops: []Op{forged}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Starts) != 1 || got.Starts[0].Stage != "review" {
		t.Fatalf("caller relabeled accepted metadata: %+v", got)
	}
	forged.Key = "aux-before"
	got, err = w.Emit(t.Context(), EmitRequest{RunID: open.RunID, Gaggle: open.Gaggle, Ops: []Op{forged}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Starts) != 0 {
		t.Fatal("ordinary auxiliary event gained start authority")
	}
}
