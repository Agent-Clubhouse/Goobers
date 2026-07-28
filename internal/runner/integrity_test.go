package runner

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestRunnerJournalsTypedIntegrityRefusal(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
			MinimumIntegrity: apiv1.IntegrityMaintainer,
			Run: &apiv1.DeterministicRun{
				Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch,
			},
		}},
	}
	machine, err := workflow.Compile(
		workflow.Definition{Name: "integrity-refusal", Version: 1, Spec: spec},
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	r, runsDir := newTestRunner(t, nil, nil)
	result, err := r.Start(context.Background(), StartInput{
		RunID: "run-integrity-refusal", Machine: machine, Gaggle: "acme-web",
		Item: &apiv1.BacklogItem{
			ID: "42", Provider: apiv1.ProviderGitHub, Labels: []string{"goobers:approved"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("Start error = %v, want integrity refusal", err)
	}
	if result.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", result.Phase)
	}

	reader, err := journal.OpenRead(runsDir + "/run-integrity-refusal")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var refusal *journal.Event
	for i := range events {
		if events[i].Error != nil && events[i].Error.Code == apiv1.IntegrityAdmissionErrorCode {
			refusal = &events[i]
		}
		if events[i].Type == journal.EventStageStarted {
			t.Fatalf("stage dispatched despite integrity refusal: %+v", events[i])
		}
	}
	if refusal == nil || refusal.Integrity != apiv1.IntegrityUnapproved ||
		refusal.MinimumIntegrity != apiv1.IntegrityMaintainer {
		t.Fatalf("typed refusal = %+v", refusal)
	}
}

func TestNormalizeArtifactIntegrityUsesRunnerOwnedAgenticGrade(t *testing.T) {
	tests := []struct {
		name     string
		taskType apiv1.TaskType
		input    apiv1.Integrity
		want     apiv1.Integrity
	}{
		{name: "agent cannot claim trusted", taskType: apiv1.TaskAgentic, input: apiv1.IntegrityTrusted, want: apiv1.IntegrityDerived},
		{name: "provider grade survives deterministic stage", taskType: apiv1.TaskDeterministic, input: apiv1.IntegrityUnapproved, want: apiv1.IntegrityUnapproved},
		{name: "missing deterministic grade defaults closed", taskType: apiv1.TaskDeterministic, want: apiv1.IntegrityDerived},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifacts := normalizeArtifactIntegrity(test.taskType, []apiv1.ArtifactPointer{{Integrity: test.input}})
			if got := artifacts[0].Integrity; got != test.want {
				t.Fatalf("integrity = %q, want %q", got, test.want)
			}
		})
	}
}
