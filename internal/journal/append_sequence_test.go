package journal

import (
	"sync/atomic"
	"testing"
)

func TestAppendWithSeqSurvivesObserverInterleaving(t *testing.T) {
	var run *Run
	var armed atomic.Bool
	var observedError error
	var err error
	run, err = Create(t.TempDir(), testIdentity(), nil, WithAppendObserver(func(_ string, _ uint64) {
		if armed.Swap(false) {
			observedError = run.Append(Event{Type: EventRunnerAnnotation, Stage: "heartbeat"})
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	armed.Store(true)
	event := Event{Type: EventStageStarted, Stage: "build", Attempt: 1, Runner: map[string]any{"artifactVisit": uint64(0)}}
	seq, err := run.AppendWithSeq(event)
	if err != nil || observedError != nil {
		t.Fatalf("append: %v / observer %v", err, observedError)
	}
	if seq+1 != run.Seq() {
		t.Fatalf("returned %d high-water %d; want exact start before interleaved append", seq, run.Seq())
	}
	reader, err := OpenRead(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range events {
		if got.Type == EventStageStarted && (got.Seq != seq || got.Runner["artifactVisit"] != float64(seq)) {
			t.Fatalf("wrong durable scope: %+v", got)
		}
	}
	if event.Runner["artifactVisit"] != uint64(0) {
		t.Fatal("append mutated caller map")
	}
}

func TestAppendWithSeqFailureReturnsNoAnchor(t *testing.T) {
	run, err := Create(t.TempDir(), testIdentity(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	// Break the actual append destination while retaining the writer lock.
	if err := run.events.Close(); err != nil {
		t.Fatal(err)
	}
	seq, err := run.AppendWithSeq(Event{Type: EventStageStarted, Stage: "build", Attempt: 1})
	if err == nil || seq != 0 {
		t.Fatalf("failed persistence exposed an anchor: seq=%d err=%v", seq, err)
	}
}
