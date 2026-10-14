package runner

import (
	"context"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// TestSelectiveParallelDesignExampleOnLocalRunner runs the design's §5.1
// example end to end on the local runner with the real shell executor: the
// triage command writes lenses.json and publishes it through the named
// artifact slot, and the parallel executes the baseline branch and exactly
// the one optional branch the selection names.
func TestSelectiveParallelDesignExampleOnLocalRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stage commands use POSIX shell syntax")
	}
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, nil, reg)
			if err != nil {
				return nil, err
			}
			return executor.NewShellExecutor(injector, rec)
		},
		Worktrees: wtMgr, RunsDir: runsDir, ScratchDir: t.TempDir(),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	const selection = `{"schemaVersion":1,"branches":[{"name":"security","reason":"touches internal/auth"}]}`
	const manifest = `{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"lenses","path":"lenses.json","mediaType":"application/json"}]}`
	shell := func(name, script, next string) apiv1.Task {
		return apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run: &apiv1.DeterministicRun{Command: []string{"sh", "-c", script}, Workspace: apiv1.WorkspaceScratch}, Next: next}
	}
	triage := shell("triage", "printf '%s' '"+selection+"' > lenses.json && printf '%s' '"+manifest+"' > manifest.json", "review")
	triage.Goal = "Decide which review lenses apply to this change."
	triage.Inputs = map[string]string{"artifactManifestFile": "manifest.json"}
	triage.ArtifactSlots = []apiv1.ArtifactSlot{{Name: "lenses", MediaType: "application/json"}}
	lens := func(name string) apiv1.Task { return shell("review-"+name, "true", workflow.TargetJoin) }
	machine, err := workflow.Compile(workflow.Definition{
		Name: "selective-review", Version: 1, DSLVersion: "3.1",
		Annotations: map[string]string{"goobers.dev/allow-preview-features": "true"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "triage",
			Tasks: []apiv1.Task{triage, lens("baseline"), lens("security"), lens("migration"), lens("ui"), shell("collate", "true", workflow.TerminalComplete)},
			Parallels: []apiv1.Parallel{{
				Name: "review", FailurePolicy: apiv1.BranchContinueOnError, BranchTimeoutSeconds: 1800,
				MaxConcurrentBranches: 3, Join: "collate",
				Selection: &apiv1.ParallelSelection{Source: apiv1.FanOutSource{Task: "triage", Slot: "lenses"}},
				Branches: []apiv1.Branch{
					{Name: "baseline", Start: "review-baseline"},
					{Name: "security", Start: "review-security", Optional: true},
					{Name: "migration", Start: "review-migration", Optional: true},
					{Name: "ui", Start: "review-ui", Optional: true},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	const runID = "selective-design-example"
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q (failure %s/%s: %s), want completed", result.Phase, result.FailureStage, result.FailureCode, result.FailureMessage)
	}
	ran := map[string]bool{}
	for _, event := range readRunEvents(t, runsDir, runID) {
		if event.Type == journal.EventStageStarted {
			ran[event.Stage] = true
		}
	}
	for stage, want := range map[string]bool{"triage": true, "review-baseline": true, "review-security": true, "review-migration": false, "review-ui": false, "collate": true} {
		if ran[stage] != want {
			t.Errorf("%s ran = %v, want %v", stage, ran[stage], want)
		}
	}
	if got := completenessSummary(parallelCompleteness(t, runsDir, runID)); !strings.HasSuffix(got, "3:migration:skipped,4:ui:skipped") ||
		!strings.HasPrefix(got, "1:baseline:") || !strings.Contains(got, "2:security:") || strings.Contains(got, "security:skipped") {
		t.Fatalf("completeness = %s", got)
	}
}
