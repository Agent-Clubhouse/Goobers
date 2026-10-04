package journal

import (
	"fmt"
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
