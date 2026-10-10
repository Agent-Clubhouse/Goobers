package journal

import (
	"testing"
	"time"
)

func TestChildWaitClocksCountOnlyWholeRunParking(t *testing.T) {
	events, wa, _ := parallelWaitFixture()
	header := wa.Runner["childWait"].(ChildWaitHeader)
	start, now := events[0].Time, events[0].Time.Add(10*time.Minute)
	for _, history := range [][]Event{events[:3], events[:4]} {
		elapsed, err := ChildExecutionElapsed(history, start, now, nil)
		if err != nil || elapsed != 10*time.Minute {
			t.Fatal("queued or running sibling stopped the run clock", elapsed, err)
		}
	}
	continued := Event{Type: EventRunnerAnnotation, Time: start.Add(8 * time.Minute), Branch: 1, Stage: wa.Stage, Attempt: 1, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": header.Request.RequestID}}
	events = append(events, continued)
	whole, err := ChildExecutionElapsed(events, start, now, nil)
	if err != nil || whole != 6*time.Minute {
		t.Fatal(whole, err)
	}
	branch := 1
	elapsed, err := ChildExecutionElapsed(events, start, now, &branch)
	if err != nil || elapsed != 4*time.Minute {
		t.Fatal(elapsed, err)
	}
	branch = 2
	elapsed, err = ChildExecutionElapsed(events, start, now, &branch)
	if err != nil || elapsed != 4*time.Minute {
		t.Fatal(elapsed, err)
	}
}
