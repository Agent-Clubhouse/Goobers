package prepare

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type sink struct{ events []journal.Event }

func (s *sink) Append(ev journal.Event) error { s.events = append(s.events, ev); return nil }

var repo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

func branch() journal.Event {
	return journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Kind: "branch", ID: "b"}}
}
func stage(name, status string) journal.Event {
	return journal.Event{Type: journal.EventStageFinished, Stage: name, Status: status}
}

func TestFinalizeBranchDecisions(t *testing.T) {
	ok := func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
		return providers.DeleteBranchResult{Deleted: true}, nil
	}
	cases := []struct {
		name         string
		events       []journal.Event
		del          DeleteFunc
		outcome, why string
	}{
		{"deleted", []journal.Event{branch(), stage("push-branch", "success")}, ok, CleanupSucceeded, ""},
		{"never pushed", []journal.Event{branch()}, ok, CleanupSkipped, "branch-not-pushed"},
		{"pr opened", []journal.Event{branch(), stage("push-branch", "success"), stage("open-pr", "success")}, ok, CleanupSkipped, "pull-request-opened"},
		{"no branch", nil, ok, CleanupSkipped, "branch-reference-missing"},
		{"no provider", []journal.Event{branch(), stage("push-branch", "success")}, nil, CleanupFailed, ""},
		{"in use", []journal.Event{branch(), stage("push-branch", "success")},
			func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
				return providers.DeleteBranchResult{}, &providers.BranchInUseError{}
			}, CleanupSkipped, "pull-request-open"},
		{"delete failed", []journal.Event{branch(), stage("push-branch", "success")},
			func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
				return providers.DeleteBranchResult{}, errors.New("boom")
			}, CleanupFailed, ""},
	}
	for _, tc := range cases {
		s := &sink{}
		if err := FinalizeBranch(tc.events, s, repo, tc.del); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(s.events) != 1 || s.events[0].Runner["outcome"] != tc.outcome || (tc.why != "" && s.events[0].Runner["reason"] != tc.why) {
			t.Errorf("%s: recorded %+v, want %s/%s", tc.name, s.events, tc.outcome, tc.why)
		}
	}
	// A finalized branch is not touched again.
	done := []journal.Event{branch(), stage("push-branch", "success")}
	done = append(done, journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Kind: "branch", ID: "b"}, Runner: map[string]any{"operation": OperationDelete}})
	if s := (&sink{}); FinalizeBranch(done, s, repo, ok) != nil || len(s.events) != 0 {
		t.Fatal("a finalized branch was finalized again")
	}
}

func TestLabelOnlyThePROpenedByTheRunOnceAndNeverForInfraFailure(t *testing.T) {
	var labeled []string
	label := func(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
		labeled = append(labeled, req.ID)
		return providers.WorkItem{}, nil
	}
	pr := func(id, op string) journal.Event {
		return journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Kind: "pr", ID: id}, Runner: map[string]any{"operation": op}}
	}
	run := func(events []journal.Event) {
		labeled = nil
		if err := LabelAbortedPR(events, "run", &sink{}, repo, label, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	run([]journal.Event{pr("9", PROpenOperation), pr("5", "label")})
	if len(labeled) != 1 || labeled[0] != "9" {
		t.Fatalf("labeled %v, want only the PR the run opened", labeled)
	}
	run([]journal.Event{pr("9", PROpenOperation), pr("9", RunAbortLabelOperation)})
	if len(labeled) != 0 {
		t.Fatalf("relabeled %v", labeled)
	}
	run([]journal.Event{pr("9", PROpenOperation), {Type: journal.EventRunFinished, TerminalCause: &journal.TerminalCause{Classification: journal.TerminalInfrastructureFailure}}})
	if len(labeled) != 0 {
		t.Fatalf("labeled an infrastructure failure's PR: %v", labeled)
	}
	if !LabelsPhase(journal.PhaseFailed) || LabelsPhase(journal.PhaseCompleted) {
		t.Fatal("LabelsPhase misclassifies")
	}
}
