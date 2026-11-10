package engine

import (
	"strings"
	"testing"
)

func TestHITLLiveTerminalNMinusOneHistoryReplays(t *testing.T) {
	// Captured from real Temporal before the terminal-transition version marker,
	// at f6f04552804c512189c1abc951b3ddb53dc1b99c.
	history := loadHistoryFixture(t, "testdata/hitl-live-terminal-n-minus-one.json")
	for _, event := range history.Events {
		if marker := event.GetMarkerRecordedEventAttributes(); marker != nil && strings.Contains(marker.String(), "hitl-live-terminal-transition-v1") {
			t.Fatal("N-1 history contains the new terminal transition marker")
		}
	}
	if count := replayEveryWorkflowTask(t, history, hitlReplay); count < 3 {
		t.Fatalf("only %d workflow task boundaries", count)
	}
}

func TestHumanGateRerunBudgetNMinusOneHistoryReplays(t *testing.T) {
	// Captured on e5296885a before human-gate-rerun-budget-v1, including a
	// committed reviewer rerun with the old repassAttempt=2 event.
	history := loadHistoryFixture(t, "testdata/hitl-reviewer-rerun-n-minus-one.json")
	for _, event := range history.Events {
		if marker := event.GetMarkerRecordedEventAttributes(); marker != nil && strings.Contains(marker.String(), humanGateRerunBudgetChange) {
			t.Fatal("N-1 history contains new budget marker")
		}
	}
	if count := replayEveryWorkflowTask(t, history, hitlReplay); count < 3 {
		t.Fatalf("only %d workflow task boundaries", count)
	}
}
