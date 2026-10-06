package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestChildStageOriginUsesAtomicCommittedSequences(t *testing.T) {
	run, _ := newRun(t)
	t.Cleanup(func() { _ = run.Close() })
	var group sync.WaitGroup
	failures := make(chan error, 16)
	for branch := 1; branch <= 16; branch++ {
		group.Add(1)
		go func() {
			defer group.Done()
			var occurrence string
			for attempt := 1; attempt <= 3; attempt++ {
				seq, origin, err := run.AppendChildStageStarted(Event{Type: EventStageStarted, Stage: "work", Branch: branch, Attempt: attempt}, attempt > 1)
				if err != nil {
					failures <- err
					return
				}
				if attempt == 1 {
					occurrence = origin.StageOccurrence
				}
				if origin.AttemptID != StageAttemptID(testIdentity().RunID, branch, "work", seq) || origin.StageOccurrence != occurrence {
					failures <- fmt.Errorf("branch %d: wrong committed identity: %+v", branch, origin)
					return
				}
				if err := run.Append(Event{Type: EventStageHeartbeat, Branch: branch, Stage: "work", Attempt: attempt}); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	reader, err := OpenRead(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		if event.Type != EventStageStarted {
			continue
		}
		origin, err := ChildWorkflowOriginForEvent(testIdentity().RunID, event)
		if err != nil || seen[origin.AttemptID] {
			t.Fatalf("duplicate or invalid committed origin: %+v %v", origin, err)
		}
		seen[origin.AttemptID] = true
	}
	if len(seen) != 48 {
		t.Fatalf("starts=%d, want 48", len(seen))
	}
}

func TestChildStageOriginRetainsOccurrenceAcrossRecoveryAndHumanRestart(t *testing.T) {
	run, _ := newRun(t)
	dir := run.Dir()
	_, first, err := run.AppendChildStageStarted(Event{Type: EventStageStarted, Stage: "work", Attempt: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	run, _, err = Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	for _, next := range []Event{
		{Type: EventStageStarted, Stage: "work", Attempt: 2, AttemptClass: AttemptInfra},
		{Type: EventStageStarted, Stage: "work", Attempt: 3, AttemptClass: AttemptPolicy},
		{Type: EventStageStarted, Stage: "work", Attempt: 4, AttemptClass: AttemptHuman},
	} {
		_, origin, err := run.AppendChildStageStarted(next, true)
		if err != nil || origin.StageOccurrence != first.StageOccurrence || origin.AttemptID == first.AttemptID {
			t.Fatalf("continuation=%+v err=%v", origin, err)
		}
	}
	_, visit, err := run.AppendChildStageStarted(Event{Type: EventStageStarted, Stage: "work", Attempt: 1}, false)
	if err != nil || visit.StageOccurrence == first.StageOccurrence || visit.AttemptID != visit.StageOccurrence {
		t.Fatalf("new visit=%+v err=%v", visit, err)
	}
}

func TestChildStageOriginRefusesUnboundHistoricalContinuation(t *testing.T) {
	for _, legacyStart := range []bool{false, true} {
		t.Run(fmt.Sprint(legacyStart), func(t *testing.T) {
			run, _ := newRun(t)
			t.Cleanup(func() { _ = run.Close() })
			if legacyStart {
				if err := run.Append(Event{Type: EventStageStarted, Stage: "work", Attempt: 1}); err != nil {
					t.Fatal(err)
				}
			}
			if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseEscalated)}); err != nil {
				t.Fatal(err)
			}
			before := run.Seq()
			if _, origin, err := run.AppendChildStageStarted(Event{Type: EventStageStarted, Stage: "work", Attempt: 2, AttemptClass: AttemptHuman}, true); err == nil || origin != nil || run.Seq() != before {
				t.Fatalf("unbound continuation appended or acquired origin: %+v %v", origin, err)
			}
		})
	}
}

func TestChildStageOriginCheckpointFailureReturnsNoUsableIdentity(t *testing.T) {
	run, _ := newRun(t)
	t.Cleanup(func() { _ = run.Close() })
	// Block checkpoint creation after the event has been durably appended.
	// The caller must not dispatch using an identity from a failed operation.
	tmp := filepath.Join(run.Dir(), fileStateTemp)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	before := run.Seq()
	seq, origin, err := run.AppendChildStageStarted(Event{Type: EventStageStarted, Stage: "work", Attempt: 1}, false)
	if err == nil || seq != 0 || origin != nil {
		t.Fatalf("failed checkpoint exposed usable origin: seq=%d origin=%+v err=%v", seq, origin, err)
	}
	if run.Seq() != before+1 {
		t.Fatal("test did not reach the post-append checkpoint failure")
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := Recover(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	_, resumed, err := recovered.AppendChildStageStarted(Event{Type: EventStageStarted, Stage: "work", Attempt: 2, AttemptClass: AttemptInfra}, true)
	if err != nil {
		t.Fatal(err)
	}
	wantOccurrence := StageAttemptID(testIdentity().RunID, 0, "work", before+1)
	if resumed.StageOccurrence != wantOccurrence || resumed.AttemptID == wantOccurrence {
		t.Fatalf("recovery did not retain the durable occurrence: %+v", resumed)
	}
}

func TestChildStageOriginRejectsReboundEvent(t *testing.T) {
	runID := testIdentity().RunID
	attempt := StageAttemptID(runID, 2, "work", 7)
	for name, mutate := range map[string]func(*Event){
		"stage":      func(e *Event) { e.Stage = "other" },
		"branch":     func(e *Event) { e.Branch++ },
		"sequence":   func(e *Event) { e.Seq++ },
		"type":       func(e *Event) { e.Type = EventStageFinished },
		"attempt":    func(e *Event) { e.Runner[ChildWorkflowAttemptKey] = StageAttemptID("other-run", 2, "work", 7) },
		"occurrence": func(e *Event) { e.Runner[ChildWorkflowOccurrenceKey] = "unbound" },
	} {
		t.Run(name, func(t *testing.T) {
			event := Event{Type: EventStageStarted, Stage: "work", Branch: 2, Seq: 7,
				Runner: map[string]any{ChildWorkflowOccurrenceKey: attempt, ChildWorkflowAttemptKey: attempt}}
			mutate(&event)
			if origin, err := ChildWorkflowOriginForEvent(runID, event); err == nil || origin != nil {
				t.Fatalf("rebound event accepted: %+v %v", origin, err)
			}
		})
	}
}
