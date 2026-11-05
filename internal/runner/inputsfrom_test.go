package runner

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestReconstructWorkspaceRevisionDeepCopiesAndRejectsConflicts(t *testing.T) {
	revision := &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{
			Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo",
		},
		CommitSHA: strings.Repeat("a", 40),
	}
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "produce", Status: "success", WorkspaceRevision: revision.DeepCopy()},
		{Type: journal.EventStageFinished, Stage: "consume", Status: "success", WorkspaceRevision: revision.DeepCopy()},
	}
	machine := workspaceRevisionResumeMachine(t)
	got, err := reconstructWorkspaceRevision(events, machine)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("reconstructWorkspaceRevision returned nil")
	}
	events[0].WorkspaceRevision.Repository.Name = "changed"
	if got.Repository.Name != "repo" {
		t.Fatal("reconstructed revision aliases journal data")
	}
	revision.CommitSHA = strings.Repeat("b", 40)
	if got.CommitSHA != strings.Repeat("a", 40) {
		t.Fatal("reconstructed revision aliases journal data")
	}
	conflict := revision.DeepCopy()
	events[1].WorkspaceRevision = conflict
	if got, err := reconstructWorkspaceRevision(events, machine); err == nil || got != nil {
		t.Fatalf("conflicting revisions reconstructed as %+v", got)
	}
}

// Resume must rebuild the same map the live walk accumulated, or a qualified
// reference resolves before a crash and fails after one.
func TestReconstructStageOutputsFromJournal(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "pr-select"},
		{Type: journal.EventStageFinished, Stage: "pr-select", Outputs: map[string]any{"selectedNumber": 7}},
		{Type: journal.EventStageFinished, Stage: "gather", Outputs: map[string]any{"files": 3}},
	}
	got := reconstructStageOutputs(events, nil)
	if got["pr-select"].Outputs["selectedNumber"] != 7 {
		t.Errorf("pr-select outputs = %#v, want selectedNumber 7", got["pr-select"])
	}
	if got["gather"].Outputs["files"] != 3 {
		t.Errorf("gather outputs = %#v, want files 3", got["gather"])
	}
}

// A repassed stage's later attempt supersedes its earlier one, matching what
// the live walk does when it re-records the stage.
func TestReconstructStageOutputsLastAttemptWins(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Outputs: map[string]any{"digest": "old"}},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Outputs: map[string]any{"digest": "new"}},
	}
	if got := reconstructStageOutputs(events, nil)["implement"].Outputs["digest"]; got != "new" {
		t.Errorf("digest = %v, want the later attempt's value", got)
	}
}

func TestReconstructStageOutputsEmptyLastAttemptWins(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Outputs: map[string]any{"digest": "old"}},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 2},
	}
	produced, seen := reconstructStageOutputs(events, nil)["implement"]
	if !seen {
		t.Fatal("implement outputs are absent, want the completed empty attempt recorded")
	}
	if len(produced.Outputs) != 0 {
		t.Fatalf("implement outputs = %#v, want empty latest attempt", produced.Outputs)
	}
}

func TestReconstructStageOutputsIsNilWhenNothingFinished(t *testing.T) {
	if got := reconstructStageOutputs([]journal.Event{{Type: journal.EventRunStarted}}, nil); got != nil {
		t.Errorf("stage outputs = %#v, want nil", got)
	}
}

func TestReconstructStageOutputsClearsContinueOnErrorFailure(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name: "continue-on-error-outputs", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "goobers",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "tolerated",
			Tasks: []apiv1.Task{
				{
					Name: "tolerated", Type: apiv1.TaskDeterministic, Goal: "tolerated",
					Run:             &apiv1.DeterministicRun{Command: []string{"true"}},
					ContinueOnError: true, Next: "strict",
				},
				{
					Name: "strict", Type: apiv1.TaskDeterministic, Goal: "strict",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
					Next: workflow.TerminalComplete,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("compile machine: %v", err)
	}
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "tolerated", Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"value": "old"}},
		{Type: journal.EventStageFinished, Stage: "tolerated", Status: string(apiv1.ResultFailure), Outputs: map[string]any{"value": "must-not-resume"}},
		{Type: journal.EventStageFinished, Stage: "strict", Status: string(apiv1.ResultFailure), Outputs: map[string]any{"value": "kept"}},
	}

	got := reconstructStageOutputs(events, machine)
	if _, ok := got["tolerated"]; ok {
		t.Fatalf("tolerated failure outputs = %#v, want absent", got["tolerated"])
	}
	if got["strict"].Outputs["value"] != "kept" {
		t.Fatalf("strict failure outputs = %#v, want preserved", got["strict"])
	}
}
