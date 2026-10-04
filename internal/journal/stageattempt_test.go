package journal

import "testing"

func TestReviewerContinuationScope(t *testing.T) {
	start := Event{Type: EventReviewerStarted, Stage: "review", Gate: "review", Attempt: 2, Branch: 1}
	for _, tc := range []struct {
		name  string
		after Event
		want  int
	}{
		{"interrupted", Event{}, 2},
		{"sibling", Event{Type: EventGateEvaluated, Gate: "review", Branch: 2}, 2},
		{"new parallel block", Event{Type: EventParallelStarted}, 0},
		{"settled", Event{Type: EventGateEvaluated, Gate: "review", Branch: 1}, 0},
		{"human rerun", Event{Type: EventStageRerunRequested, Stage: "review", Branch: 1}, 0},
		{"terminal", Event{Type: EventRunFinished}, 0},
		{"completed dispatch before routing crash", Event{Type: EventReviewerFinished, Gate: "review", Stage: "review", Attempt: 2, Branch: 1}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReviewerContinuation([]Event{start, tc.after}, "review", 1); got != tc.want {
				t.Fatalf("continuation=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestReviewerAgentAddressesUseDispatchStartAnchor(t *testing.T) {
	events := []Event{
		{Type: EventGateStarted, Gate: "review", Seq: 1, Runner: map[string]any{"repassAttempt": float64(1)}},
		{Type: EventReviewerStarted, Stage: "review", Gate: "review", Attempt: 1, Seq: 2},
		{Type: EventReviewerFinished, Stage: "review", Gate: "review", Attempt: 1, Seq: 4},
		{Type: EventReviewerStarted, Stage: "review", Gate: "review", Attempt: 2, Seq: 5},
	}
	spans, latest := collectAttemptSpans(events)
	event := Event{Seq: 6, Agent: &AgentProvenance{Stage: "review", Attempt: 2}}
	span, ok := attemptSpanForEvent(spans["review"], event)
	if !ok || span.startedSeq != 5 || latest["review"] != 5 {
		t.Fatalf("retry anchor=%+v latest=%v", span, latest)
	}
	event.Seq = 3
	event.Agent.Attempt = 1
	span, ok = attemptSpanForEvent(spans["review"], event)
	if !ok || span.startedSeq != 2 || span.finishedSeq != 4 {
		t.Fatalf("first dispatch anchor=%+v", span)
	}
}
