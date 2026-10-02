package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestRunnerPinsNamedArtifactPublicationToJournalVisit(t *testing.T) {
	spec := apiv1.WorkflowSpec{Gaggle: "acme-web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "produce", Tasks: []apiv1.Task{{Name: "produce", Type: apiv1.TaskDeterministic, Goal: "publish", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, ArtifactSlots: []apiv1.ArtifactSlot{{Name: "report"}}, Next: workflow.TerminalComplete}}}
	machine, err := workflow.Compile(workflow.Definition{Name: "slots", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	runsDir, repo, manager := newTestRunnerEnv(t)
	var captured *outputCapturingDeterministic
	r, err := New(Config{NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		captured = &outputCapturingDeterministic{rec: rec, byTask: map[string]stubTaskResult{"slots:produce": {status: apiv1.ResultSuccess}}}
		return captured, nil
	}, Worktrees: manager, RunsDir: runsDir, RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Start(t.Context(), StartInput{RunID: "slots", Machine: machine, Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	env := captured.received["slots:produce"]
	if env.ArtifactPublication == nil || env.ArtifactPublication.Stage != "produce" || env.ArtifactPublication.Visit == 0 || env.Attempt != 1 || env.ArtifactPublication.Slots[0].Name != "report" {
		t.Fatalf("envelope=%+v", env)
	}
	found := false
	for _, event := range readRunEvents(t, runsDir, "slots") {
		if event.Type == journal.EventStageStarted && event.Stage == "produce" {
			found = true
			if event.Runner["artifactVisit"] != float64(env.ArtifactPublication.Visit) {
				t.Fatalf("journal visit=%v contract=%+v", event.Runner, env.ArtifactPublication)
			}
		}
	}
	if !found {
		t.Fatal("missing started event")
	}
}
