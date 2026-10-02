package nowork

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestNoWorkTerminalIgnoresSuccessfulStages guards the classification itself:
// only a literal no-work stage status counts, so an ordinary successful run is
// treated as productive.
func TestNoWorkTerminalIgnoresSuccessfulStages(t *testing.T) {
	_, isNoWork := TerminalFromEvents([]journal.Event{{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultSuccess)}}, "implement")
	if isNoWork {
		t.Fatal("a successful stage must not be classified as a no-work terminal")
	}
}

// TestParallelBranchNoWorkDoesNotCountAsRunVerdict is the regression for the
// fan-out misclassification: branch stages append to the SAME run journal, and
// a branch that returns no-work ends only that branch while its siblings keep
// producing. Counting that as the run's verdict would park a healthy item.
func TestParallelBranchNoWorkDoesNotCountAsRunVerdict(t *testing.T) {
	_, isNoWork := TerminalFromEvents([]journal.Event{
		{Type: journal.EventStageFinished, Stage: "security", Branch: 1, Status: string(apiv1.ResultNoWork)},
		{Type: journal.EventStageFinished, Stage: "performance", Branch: 2, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventStageFinished, Stage: "collate", Branch: 0, Status: string(apiv1.ResultSuccess)},
	}, "collate")
	if isNoWork {
		t.Fatal("a branch's no-work verdict must not be treated as the run's verdict")
	}
}

// TestStaleEarlierNoWorkDoesNotCountWhenRunEndedProductively is the regression
// for the #5107 repass: a no-work event stays in the journal forever, so a run
// that later produced a pull request must not be classified by it.
func TestStaleEarlierNoWorkDoesNotCountWhenRunEndedProductively(t *testing.T) {
	_, isNoWork := TerminalFromEvents([]journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultNoWork)},
		{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventStageFinished, Stage: "close-out", Status: string(apiv1.ResultSuccess)},
	}, "close-out")
	if isNoWork {
		t.Fatal("a stale earlier no-work must not classify a run that ended productively")
	}
}

func TestTerminalFromEventsKeepsNewestMatchingVerdict(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultNoWork), Outputs: map[string]any{"reason": "old"}},
		{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultNoWork), Outputs: map[string]any{"status": " already-fixed ", "noWorkReason": " fixed ", "reason": "fallback", "existingCommit": " abc123 "}},
		{Type: journal.EventStageFinished, Stage: "implement", Branch: 1, Status: string(apiv1.ResultNoWork), Outputs: map[string]any{"reason": "branch"}},
		{Type: journal.EventStageStarted, Stage: "implement", Status: string(apiv1.ResultNoWork)},
	}
	got, ok := TerminalFromEvents(events, "implement")
	want := Terminal{Stage: "implement", Reason: "fixed", Verdict: "already-fixed", Evidence: "abc123"}
	if !ok || got != want {
		t.Fatalf("terminal = %+v, %v; want %+v, true", got, ok, want)
	}
	if got, ok := TerminalFromEvents(events, ""); ok || got != (Terminal{}) {
		t.Fatalf("empty final state: %+v, %v", got, ok)
	}
}

func TestTerminalReasonFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outputs map[string]any
		want    string
	}{
		{name: "absent", want: ""},
		{name: "fallback", outputs: map[string]any{"noWorkReason": " ", "reason": " fallback "}, want: "fallback"},
		{name: "non-string", outputs: map[string]any{"noWorkReason": 42, "reason": false}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TerminalFromEvents([]journal.Event{{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultNoWork), Outputs: tc.outputs}}, "implement")
			if !ok || got.Reason != tc.want {
				t.Fatalf("terminal = %+v, %v; want reason %q", got, ok, tc.want)
			}
		})
	}
}
