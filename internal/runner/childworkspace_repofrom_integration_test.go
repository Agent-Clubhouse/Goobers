//go:build integration

package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationChildWorkspaceSerialRepoFrom(t *testing.T) {
	testdep.Require(t, "git")
	for _, pause := range []bool{false, true} {
		name := "direct"
		if pause {
			name = "human-resume"
		}
		t.Run(name, func(t *testing.T) {
			f := prepareChildWorkspaceFixture(t, false)
			f.input.Machine = childRepoFromMachine(t, pause)
			producerHead := ""
			calls := []string{}
			invoke := func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				calls = append(calls, env.TaskID)
				if env.Workspace != f.child.Path {
					t.Fatalf("stage %s replaced the admitted workspace", env.TaskID)
				}
				switch env.TaskID {
				case f.input.RunID + ":produce":
					childWorkspaceRead(t, env.Workspace, "main.txt", []byte("parent dirty\n"))
					childWorkspaceWrite(t, env.Workspace, "main.txt", []byte("child commit\n"))
					childWorkspaceWrite(t, env.Workspace, "child.bin", []byte{0, 1, 255})
					runGit(t, env.Workspace, "add", "main.txt", "child.bin")
					runGit(t, env.Workspace, "-c", "user.name=Child Test", "-c", "user.email=child-test@example.invalid", "commit", "-m", "child producer")
					producerHead = gitOutput(t, env.Workspace, "rev-parse", "HEAD")
				case f.input.RunID + ":consume":
					if producerHead == "" || gitOutput(t, env.Workspace, "rev-parse", "HEAD") != producerHead {
						t.Fatal("repoFrom consumer lost the producer commit")
					}
					childWorkspaceRead(t, env.Workspace, "main.txt", []byte("child commit\n"))
					childWorkspaceRead(t, env.Workspace, "child.bin", []byte{0, 1, 255})
					childWorkspaceRead(t, env.Workspace, "parent.bin", []byte{0, 255, 1})
				default:
					t.Fatalf("unexpected task %s", env.TaskID)
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}
			result, err := f.runner(t, invoke).Start(t.Context(), f.input)
			if err != nil {
				t.Fatal(err)
			}
			if pause {
				if result.FinalState != "approval" || len(calls) != 1 {
					t.Fatalf("pause=%+v calls=%v", result, calls)
				}
				in := humanResumeInput(f.input.RunID, f.input.Machine, "approval", latestHumanPauseSeq(t, f.config.RunsDir, f.input.RunID, "approval"), "pass")
				in.GooberDigest = f.input.GooberDigest
				result, err = f.runner(t, invoke).Resume(t.Context(), in)
			}
			if err != nil || result.Phase != journal.PhaseCompleted || len(calls) != 2 {
				t.Fatalf("result=%+v calls=%v error=%v", result, calls, err)
			}
			f.assertParentUnchanged(t)
		})
	}
}

func childRepoFromMachine(t *testing.T, pause bool) *workflow.Machine {
	t.Helper()
	next := "consume"
	if pause {
		next = "approval"
	}
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Start: "produce", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Tasks: []apiv1.Task{
			{Name: "produce", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "commit a change", Workspace: apiv1.WorkspaceRepo, Next: next},
			{Name: "consume", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "inspect the producer commit", Workspace: apiv1.WorkspaceRepo, RepoFrom: apiv1.RepoFrom{"produce"}},
		},
	}
	if pause {
		spec.Gates = []apiv1.Gate{{Name: "approval", Evaluator: apiv1.EvaluatorHuman, Human: &apiv1.HumanGate{}, Branches: map[string]string{"pass": "consume", "reject": workflow.TargetAbort}}}
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "generated-child-handoff", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}
