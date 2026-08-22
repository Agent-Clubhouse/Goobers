package runner

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// TestRunnerExecutesDSL30Machine is the mode-1 acceptance evidence for issue
// #3505: a DSL 3.0 document — runsOn blocks, a commitsRepo producer, and a
// declared repoFrom edge — compiles through the version router into a Machine
// the local runner executes end to end. Placement requirements are compiled
// data the mode-3 constraint solve will consume (#3506 onward); on the local
// runner they are advisory by design (dsl-3.0.md D4), so the run completes on
// whatever host executes this test.
func TestRunnerExecutesDSL30Machine(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name: "dsl30-linear", Version: 1, DSLVersion: "3.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "goobers",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "seed",
			Tasks: []apiv1.Task{
				{
					Name: "seed", Type: apiv1.TaskDeterministic, Goal: "commit the change",
					Run:         &apiv1.DeterministicRun{Command: []string{"true"}},
					CommitsRepo: true,
					RunsOn: &apiv1.RunsOn{
						OS: "linux", CPU: "2000m", Memory: "4Gi",
						Capabilities: []string{"go@1.26", "make"},
					},
					Next: "local-ci",
				},
				{
					Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "run CI on the handed-off head",
					Run:      &apiv1.DeterministicRun{Command: []string{"true"}},
					RepoFrom: apiv1.RepoFrom{"seed"},
					RunsOn:   &apiv1.RunsOn{Capabilities: []string{"go@1.26"}},
					Next:     "report",
				},
				{
					Name: "report", Type: apiv1.TaskDeterministic, Goal: "report",
					Run:    &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					RunsOn: &apiv1.RunsOn{Restrictions: []string{"network:none"}},
				},
			},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile 3.0 machine: %v", err)
	}

	results := map[string]stubTaskResult{
		"dsl30:seed":     {status: apiv1.ResultSuccess},
		"dsl30:local-ci": {status: apiv1.ResultSuccess},
		"dsl30:report":   {status: apiv1.ResultSuccess},
	}
	r, _ := newTestRunner(t, results, nil)
	r.cfg.ScratchDir = t.TempDir()

	result, err := r.Start(context.Background(), StartInput{
		RunID: "dsl30", Machine: machine, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
}
