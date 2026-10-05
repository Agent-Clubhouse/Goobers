package runner

import (
	"fmt"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func selectionContribution(seq uint64, branch int, stage, workspace string) journal.Event {
	digest := journal.Digest([]byte(fmt.Sprint(seq)))
	value := parentContribution{Version: 1, ContractDigest: digest, Custody: ContainedParentWorkspaceCustody{Version: 1, Origin: &apiv1.ChildWorkflowOrigin{StageOccurrence: stage, AttemptID: stage}, Workspace: worktree.StageCustody{WorkspaceID: workspace}}, Output: journal.Ref{Path: "artifact", Digest: digest, Size: 1}}
	return journal.Event{Seq: seq, Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: 1, Branch: branch, Runner: map[string]any{"kind": ParentContributionKind, "contractDigest": digest, "contribution": value}}
}

func TestParentContributionSelectionRespectsForkAndCompletionOrder(t *testing.T) {
	finished := func(seq uint64, branch int, stage string) journal.Event {
		return journal.Event{Seq: seq, Type: journal.EventStageFinished, Branch: branch, Stage: stage, Attempt: 1}
	}
	events := []journal.Event{selectionContribution(1, 0, "seed", "root"), finished(2, 0, "seed"), {Seq: 3, Type: journal.EventParallelStarted}, selectionContribution(4, 1, "a", "first-a"), selectionContribution(5, 2, "b", "first-b"), finished(6, 2, "b"), finished(7, 1, "a")}
	for _, tc := range []struct {
		branch  int
		task    string
		sources apiv1.RepoFrom
		want    string
		fork    bool
	}{
		{1, "next-a", apiv1.RepoFrom{"a"}, "a", false},
		{2, "b", apiv1.RepoFrom{"seed"}, "b", false},
		{3, "c", apiv1.RepoFrom{"seed"}, "seed", true},
		{0, "join", apiv1.RepoFrom{"a", "b", "seed"}, "a", false},
	} {
		selected, fork, err := selectParentContribution(events, apiv1.Task{Name: tc.task, RepoFrom: tc.sources}, tc.branch)
		if err != nil || selected.Stage != tc.want || fork != tc.fork {
			t.Fatal(tc, selected, fork, err)
		}
	}
	if _, _, err := selectParentContribution(events, apiv1.Task{Name: "join", RepoFrom: apiv1.RepoFrom{"b"}}, 0); err == nil {
		t.Fatal("undeclared latest producer was skipped")
	}
	events = append(events, journal.Event{Seq: 8, Type: journal.EventParallelFinished}, journal.Event{Seq: 9, Type: journal.EventParallelStarted})
	selected, fork, err := selectParentContribution(events, apiv1.Task{Name: "a", RepoFrom: apiv1.RepoFrom{"a", "b"}}, 1)
	if err != nil || selected.Seq != 4 || !fork {
		t.Fatal("later occurrence reused an earlier branch", selected, fork, err)
	}
	// Inventory keys physical workspaces, not the branch ordinal reused later.
	events = append(events, selectionContribution(10, 1, "a", "second-a"))
	latest, err := latestParentContributions(events)
	if err != nil || len(latest) != 4 {
		t.Fatal("earlier branch contribution disappeared", latest, err)
	}
}

func TestParentForkBudgetReservesAllDeclaredSiblings(t *testing.T) {
	var events []journal.Event
	for i := 0; i < maxParentForks-1; i++ {
		workspace := fmt.Sprint("old-", i)
		value := ContainedParentWorkspaceCustody{Version: 1, Origin: &apiv1.ChildWorkflowOrigin{StageOccurrence: "old", AttemptID: "old"}, Workspace: worktree.StageCustody{WorkspaceID: workspace}}
		events = append(events, journal.Event{Seq: uint64(i + 1), Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": ContainedParentWorkspaceKind, "custody": value}})
	}
	events = append(events, journal.Event{Seq: 600, Type: journal.EventParallelStarted})
	if err := parentParallelForkBudget("run", 1, events); err != nil {
		t.Fatal(err)
	}
	if err := parentParallelForkBudget("run", 2, events); err == nil {
		t.Fatal("concurrent branches could overshoot the hard bound")
	}
}
