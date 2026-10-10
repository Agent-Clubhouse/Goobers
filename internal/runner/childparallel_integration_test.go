//go:build integration

package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type childParallelGoober struct {
	call func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error)
}

func (g childParallelGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if done := invoke.RegisterWorkspaceWriter(ctx); done != nil {
		defer done(nil)
	}
	return g.call(ctx, env)
}
func (childParallelGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	panic("unexpected reviewer")
}

func TestIntegrationGeneratedChildParallelReadOnlyViews(t *testing.T) {
	testdep.Require(t, "git")
	f := prepareChildWorkspaceFixture(t, false)
	tasks := []apiv1.Task{}
	for _, name := range []string{"inspect-a", "inspect-b"} {
		tasks = append(tasks, apiv1.Task{Name: name, Type: apiv1.TaskAgentic, Goober: "coder", Goal: "inspect pinned fork", Workspace: apiv1.WorkspaceRepoReadOnly, Next: workflow.TargetJoin})
	}
	tasks = append(tasks, apiv1.Task{Name: "collate", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "collate branch outputs", Workspace: apiv1.WorkspaceScratch})
	spec := apiv1.WorkflowSpec{Gaggle: "web", Start: "fan", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Tasks: tasks,
		Parallels: []apiv1.Parallel{{Name: "fan", Join: "collate", MaxConcurrentBranches: 2, FailurePolicy: apiv1.BranchAllOrNothing, OnFailure: workflow.TargetAbort, Branches: []apiv1.Branch{{Name: "a", Start: "inspect-a"}, {Name: "b", Start: "inspect-b"}}}}}
	machine, err := workflow.Compile(workflow.Definition{Name: "generated-child", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	f.input.Machine = machine
	var mu sync.Mutex
	views := map[string]string{}
	arrived := make(chan struct{})
	joined := false
	f.config.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return childParallelGoober{call: func(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			stage := strings.TrimPrefix(env.TaskID, f.input.RunID+":")
			if stage == "collate" {
				mu.Lock()
				defer mu.Unlock()
				if len(views) != 2 || views["inspect-a"] == views["inspect-b"] {
					return apiv1.ResultEnvelope{}, errors.New("child branches did not have distinct views")
				}
				if env.Inputs[BranchCompletenessInput] == nil {
					return apiv1.ResultEnvelope{}, errors.New("join lost branch completeness")
				}
				joined = true
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}
			childWorkspaceRead(t, env.Workspace, "main.txt", []byte("parent dirty\n"))
			mu.Lock()
			views[stage] = env.Workspace
			if len(views) == 2 {
				close(arrived)
			}
			mu.Unlock()
			select {
			case <-arrived:
			case <-ctx.Done():
				return apiv1.ResultEnvelope{}, ctx.Err()
			case <-time.After(10 * time.Second):
				return apiv1.ResultEnvelope{}, errors.New("read-only child branches were serialized")
			}
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"inspection": stage}}, nil
		}}, nil
	}
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Start(t.Context(), f.input)
	if err != nil || result.Phase != journal.PhaseCompleted || !joined {
		t.Fatal(result, err, joined)
	}
	f.assertParentUnchanged(t)
}
