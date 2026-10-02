package livejournal

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestStartAcknowledgmentInterleavingDedupAndRestart(t *testing.T) {
	w, runsDir := testWriter(t, WithControllerStartAuthority(testStartAuthority{}))
	at := time.Now()
	open := openBatch("start-ack", at)
	if _, err := w.EmitController(t.Context(), open); err != nil {
		t.Fatal(err)
	}
	start := appendOp("review-start", at, journal.ReviewerAttemptEvent(journal.EventReviewerStarted, "review", 2, journal.AttemptInfra))
	batch := EmitRequest{RunID: open.RunID, Gaggle: open.Gaggle, Ops: []Op{appendOp("aux-before", at, journal.Event{Type: journal.EventRunnerAnnotation}), start, appendOp("aux-after", at, journal.Event{Type: journal.EventRunnerAnnotation})}}
	first, err := w.EmitController(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Starts) != 1 || first.Starts[0].Seq != 4 || first.Seq != 5 {
		t.Fatalf("ack=%+v", first)
	}
	w.Close()
	w, err = NewWriter(func(string) (string, bool) { return runsDir, true }, WithControllerStartAuthority(testStartAuthority{}))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.EmitController(t.Context(), EmitRequest{RunID: open.RunID, Gaggle: open.Gaggle, Ops: []Op{appendOp("later-aux", at, journal.Event{Type: journal.EventRunnerAnnotation})}}); err != nil {
		t.Fatal(err)
	}
	again, err := w.EmitController(t.Context(), batch)
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
	got, err := w.EmitController(t.Context(), EmitRequest{RunID: open.RunID, Gaggle: open.Gaggle, Ops: []Op{forged}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Starts) != 1 || got.Starts[0].Stage != "review" {
		t.Fatalf("caller relabeled accepted metadata: %+v", got)
	}
	forged.Key = "aux-before"
	got, err = w.EmitController(t.Context(), EmitRequest{RunID: open.RunID, Gaggle: open.Gaggle, Ops: []Op{forged}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Starts) != 0 {
		t.Fatal("ordinary auxiliary event gained start authority")
	}
}

// A deterministic fixture; real MAC boundaries are exercised in podauth.
type testStartAuthority struct{}

func (testStartAuthority) SealControllerStart(run, key string, e journal.Event) string {
	return fmt.Sprintf("%s/%s/%d/%s/%s/%d/%d/%s", run, key, e.Seq, e.Type, e.Stage, e.Branch, e.Attempt, e.AttemptClass)
}
func (a testStartAuthority) VerifyControllerStart(run, key string, e journal.Event, proof string) bool {
	return proof == a.SealControllerStart(run, key, e)
}

func TestStartAcknowledgmentRejectsLegacyUnverifiedMarkers(t *testing.T) {
	w, runsDir := testWriter(t)
	batch := openBatch("legacy-origin", time.Now())
	if _, err := w.Emit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	// Model an existing pre-upgrade file: arbitrary Runner fields were accepted
	// without controller provenance. Reopening must not infer authority from shape.
	legacy := journal.Event{Type: journal.EventReviewerStarted, Stage: "review", Attempt: 1, Runner: map[string]any{EmitKeyRunnerField: "legacy-review", ControllerStartProofField: "true"}}
	if err := w.open[batch.RunID].jr.Append(legacy); err != nil {
		t.Fatal(err)
	}
	w.Close()
	w, err := NewWriter(func(string) (string, bool) { return runsDir, true }, WithControllerStartAuthority(testStartAuthority{}))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	req := EmitRequest{RunID: batch.RunID, Gaggle: batch.Gaggle, Ops: []Op{appendOp("legacy-review", time.Now(), legacy), batch.Ops[1]}}
	got, err := w.EmitController(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Starts) != 0 {
		t.Fatalf("legacy fields gained controller origin: %+v", got.Starts)
	}
}
