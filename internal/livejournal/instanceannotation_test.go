package livejournal

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// instanceannotation_test.go is Goobers#3898's writer-side evidence: the
// scheduler's cross-run narration reaches the daemon's INSTANCE log through
// the same run-scoped emit route a stage pod already holds a bearer for, and
// a pod can only ever annotate for its own run.

// recordingInstanceLog captures appends without a scheduler directory.
type recordingInstanceLog struct {
	mu     sync.Mutex
	events []journal.Event
	err    error
}

func (l *recordingInstanceLog) Append(ev journal.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.events = append(l.events, ev)
	return nil
}

func (l *recordingInstanceLog) snapshot() []journal.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]journal.Event(nil), l.events...)
}

func annotationOp(key string, ev journal.Event) Op {
	e := ev
	return Op{Kind: OpInstanceAnnotation, Key: key, Event: &e, Time: time.Unix(1700000000, 0).UTC()}
}

// The happy path: an annotation op lands in the instance log, typed and
// stamped, and does NOT land in the run journal (the instance log is a
// separate, cross-run record — putting scheduler narration into a run's own
// journal would corrupt that run's projection).
func TestInstanceAnnotationLandsInTheInstanceLog(t *testing.T) {
	log := &recordingInstanceLog{}
	w, runsDir := testWriter(t, WithInstanceLog(log))
	at := time.Unix(1700000000, 0).UTC()
	if _, err := w.Emit(context.Background(), openBatch("run-1", at)); err != nil {
		t.Fatalf("open: %v", err)
	}
	beforeRunEvents := len(readEvents(t, runsDir, "run-1"))

	_, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-1", Gaggle: "web",
		Ops: []Op{annotationOp("ann-1", journal.Event{
			Reason: "blocked-eligibility-skip",
			Runner: map[string]any{"item": "Agent-Clubhouse/Goobers#42", "blocked_by": "#41"},
		})},
	})
	if err != nil {
		t.Fatalf("emit annotation: %v", err)
	}

	got := log.snapshot()
	if len(got) != 1 {
		t.Fatalf("instance log events = %d, want 1", len(got))
	}
	if got[0].Type != journal.EventRunnerAnnotation {
		t.Errorf("type = %q, want %q (defaulted by the writer)", got[0].Type, journal.EventRunnerAnnotation)
	}
	if got[0].RunID != "run-1" {
		t.Errorf("run id = %q, want run-1", got[0].RunID)
	}
	if got[0].Reason != "blocked-eligibility-skip" {
		t.Errorf("reason = %q, want the caller's", got[0].Reason)
	}
	if got[0].Runner["item"] != "Agent-Clubhouse/Goobers#42" {
		t.Errorf("runner payload lost: %#v", got[0].Runner)
	}
	if after := len(readEvents(t, runsDir, "run-1")); after != beforeRunEvents {
		t.Errorf("run journal grew from %d to %d events; an instance annotation must not enter a run's own journal", beforeRunEvents, after)
	}
}

// FOREIGN-RUN REFUSAL, writer half. A pod legitimately emitting for its own
// run must not be able to attribute an instance-log entry to a DIFFERENT run
// by putting that run's id inside the event payload — the instance log is the
// cross-run record an operator reads to understand scheduling, and a stage
// that can forge entries in it can frame another run.
//
// The route refuses a principal whose subject is not the run in the path;
// this is the independent second check, on the field the route never inspects.
func TestInstanceAnnotationCannotAttributeToAForeignRun(t *testing.T) {
	log := &recordingInstanceLog{}
	w, _ := testWriter(t, WithInstanceLog(log))
	at := time.Unix(1700000000, 0).UTC()
	if _, err := w.Emit(context.Background(), openBatch("run-1", at)); err != nil {
		t.Fatalf("open: %v", err)
	}

	_, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-1", Gaggle: "web",
		Ops: []Op{annotationOp("ann-forged", journal.Event{
			RunID:  "run-victim",
			Reason: "blocked-eligibility-skip",
		})},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := log.snapshot()
	if len(got) != 1 {
		t.Fatalf("instance log events = %d, want 1", len(got))
	}
	if got[0].RunID != "run-1" {
		t.Fatalf("instance log entry attributed to %q; the writer must restamp it from the REQUEST, not the payload", got[0].RunID)
	}
}

// Fail closed when the daemon was assembled without an instance log. The
// alternative is worse than an error: a stage would report a delivered
// annotation that no operator will ever read.
func TestInstanceAnnotationWithoutAnInstanceLogIsRefused(t *testing.T) {
	w, _ := testWriter(t)
	at := time.Unix(1700000000, 0).UTC()
	if _, err := w.Emit(context.Background(), openBatch("run-1", at)); err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-1", Gaggle: "web",
		Ops: []Op{annotationOp("ann-1", journal.Event{Reason: "blocked"})},
	})
	if !errors.Is(err, ErrNoInstanceLog) {
		t.Fatalf("err = %v, want ErrNoInstanceLog", err)
	}
}

// An op with no event is a malformed op, not an empty annotation.
func TestInstanceAnnotationWithoutAnEventIsRefused(t *testing.T) {
	log := &recordingInstanceLog{}
	w, _ := testWriter(t, WithInstanceLog(log))
	at := time.Unix(1700000000, 0).UTC()
	if _, err := w.Emit(context.Background(), openBatch("run-1", at)); err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-1", Gaggle: "web",
		Ops: []Op{{Kind: OpInstanceAnnotation, Key: "ann-1"}},
	})
	if err == nil {
		t.Fatal("an annotation op carrying no event was accepted")
	}
	if len(log.snapshot()) != 0 {
		t.Fatal("a malformed op reached the instance log")
	}
}

// An instance-log write failure surfaces to the caller. The stage decides
// whether its annotation was best-effort; the plane does not decide for it.
func TestInstanceAnnotationSurfacesAppendFailures(t *testing.T) {
	log := &recordingInstanceLog{err: errors.New("disk full")}
	w, _ := testWriter(t, WithInstanceLog(log))
	at := time.Unix(1700000000, 0).UTC()
	if _, err := w.Emit(context.Background(), openBatch("run-1", at)); err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-1", Gaggle: "web",
		Ops: []Op{annotationOp("ann-1", journal.Event{Reason: "blocked"})},
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the instance log's failure", err)
	}
}

// Idempotency by key, for as long as the run stays open: a retried emit of
// the same annotation appends once. (The documented limit — a rehydrated run
// re-applies a replayed key, because deriveDedupState rebuilds from run
// journal events and these are not — is narration noise, not a correctness
// failure, and is asserted below so the boundary is recorded rather than
// discovered.)
func TestInstanceAnnotationDedupsWhileTheRunIsOpen(t *testing.T) {
	log := &recordingInstanceLog{}
	w, _ := testWriter(t, WithInstanceLog(log))
	at := time.Unix(1700000000, 0).UTC()
	if _, err := w.Emit(context.Background(), openBatch("run-1", at)); err != nil {
		t.Fatalf("open: %v", err)
	}
	batch := EmitRequest{
		RunID: "run-1", Gaggle: "web",
		Ops: []Op{annotationOp("ann-1", journal.Event{Reason: "blocked"})},
	}
	for i := 0; i < 3; i++ {
		if _, err := w.Emit(context.Background(), batch); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	if got := len(log.snapshot()); got != 1 {
		t.Fatalf("instance log events = %d after 3 identical emits, want 1", got)
	}
	// Two DIFFERENT keys are two annotations, even with identical content —
	// the same backlog item skipped as blocked on two passes is two facts.
	batch.Ops = []Op{annotationOp("ann-2", journal.Event{Reason: "blocked"})}
	if _, err := w.Emit(context.Background(), batch); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := len(log.snapshot()); got != 2 {
		t.Fatalf("instance log events = %d, want 2 distinct annotations", got)
	}
}

// The emit key is carried onto the written event, so an operator reading the
// instance log can correlate an entry with the op that produced it.
func TestInstanceAnnotationCarriesItsEmitKey(t *testing.T) {
	log := &recordingInstanceLog{}
	w, _ := testWriter(t, WithInstanceLog(log))
	at := time.Unix(1700000000, 0).UTC()
	if _, err := w.Emit(context.Background(), openBatch("run-1", at)); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-1", Gaggle: "web",
		Ops: []Op{annotationOp("instance-annotation:abc123", journal.Event{Reason: "blocked"})},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := log.snapshot()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Runner == nil {
		t.Fatal("the emit key was not recorded on the event")
	}
	found := false
	for _, v := range got[0].Runner {
		if s, ok := v.(string); ok && s == "instance-annotation:abc123" {
			found = true
		}
	}
	if !found {
		t.Errorf("runner payload %#v does not carry the emit key", got[0].Runner)
	}
}

// A batch that mixes an annotation with ordinary run-journal appends applies
// both — the claiming path emits its narration alongside real run events, and
// a writer that could only do one kind per batch would force it to choose.
func TestInstanceAnnotationCoexistsWithRunJournalOps(t *testing.T) {
	log := &recordingInstanceLog{}
	w, runsDir := testWriter(t, WithInstanceLog(log))
	at := time.Unix(1700000000, 0).UTC()
	if _, err := w.Emit(context.Background(), openBatch("run-1", at)); err != nil {
		t.Fatalf("open: %v", err)
	}
	before := len(readEvents(t, runsDir, "run-1"))
	if _, err := w.Emit(context.Background(), EmitRequest{
		RunID: "run-1", Gaggle: "web",
		Ops: []Op{
			annotationOp("ann-1", journal.Event{Reason: "blocked"}),
			appendOp("run-1|0|build|1|9", at.Add(time.Minute), journal.Event{Type: journal.EventStageFinished, Stage: "build", Attempt: 1, Status: "success"}),
		},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := len(log.snapshot()); got != 1 {
		t.Fatalf("instance log events = %d, want 1", got)
	}
	if got := len(readEvents(t, runsDir, "run-1")); got != before+1 {
		t.Fatalf("run journal events = %d, want %d", got, before+1)
	}
}
