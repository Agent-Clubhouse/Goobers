package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type capturingImplementer struct {
	env apiv1.InvocationEnvelope
}

func (c *capturingImplementer) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	c.env = env
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (c *capturingImplementer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestRunnerFirstPassImplementationContextIsJournalInspectable(t *testing.T) {
	const (
		runID       = "run-first-pass-context"
		contextJSON = `{
			"reviewerVerdictTaxonomy":{"findingClasses":[{"class":"conflict"}]},
			"hotFileMap":{"files":[{"path":"internal/runner/run.go","pullRequests":[10,11]}]}
		}`
	)
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "gather-implement-context",
		Tasks: []apiv1.Task{
			{
				Name: "gather-implement-context", Type: apiv1.TaskDeterministic,
				Goal: "gather context", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "implement",
			},
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer", Goal: "implement"},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "implementation-context", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	implementer := &capturingImplementer{}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				runID + ":gather-implement-context": {
					status:            apiv1.ResultSuccess,
					artifactName:      "implementation-context.json",
					artifactData:      []byte(contextJSON),
					artifactMediaType: "application/json",
				},
			}}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return implementer, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}

	var contextPointer *apiv1.ContextPointer
	for i := range implementer.env.ContextPointers {
		if implementer.env.ContextPointers[i].Name == "gather-implement-context.artifact[0]" {
			contextPointer = &implementer.env.ContextPointers[i]
			break
		}
	}
	if contextPointer == nil || contextPointer.Artifact == nil {
		t.Fatalf("first-pass implement context pointers = %+v, want gather-implement-context artifact", implementer.env.ContextPointers)
	}
	resolved, err := contextPointer.Artifact.Resolve(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("resolve implementation context: %v", err)
	}
	var contextBlocks map[string]json.RawMessage
	if err := json.Unmarshal(resolved, &contextBlocks); err != nil {
		t.Fatalf("unmarshal implementation context: %v", err)
	}
	for _, block := range []string{"reviewerVerdictTaxonomy", "hotFileMap"} {
		if len(contextBlocks[block]) == 0 {
			t.Fatalf("implementation context is missing %q: %s", block, resolved)
		}
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var manifestPointer *apiv1.ContextPointer
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || event.Name != "context/implement-attempt-1.json" || event.Ref == nil {
			continue
		}
		data, err := rd.ArtifactBytes(*event.Ref)
		if err != nil {
			t.Fatalf("read implement context manifest: %v", err)
		}
		var manifest contextManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("unmarshal implement context manifest: %v", err)
		}
		for i := range manifest.ContextPointers {
			if manifest.ContextPointers[i].Name == contextPointer.Name {
				manifestPointer = &manifest.ContextPointers[i]
				break
			}
		}
	}
	if manifestPointer == nil || !reflect.DeepEqual(*manifestPointer, *contextPointer) {
		t.Fatalf("journaled implementation context pointer = %+v, want %+v", manifestPointer, contextPointer)
	}
}
