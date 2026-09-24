package runner

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/internal/worktree"
)

func TestRunnerWorkspaceRevisionLivePropagationAndIdempotence(t *testing.T) {
	for _, owner := range []string{"acme", "other"} {
		t.Run(owner, func(t *testing.T) {
			def := workspaceRevisionResumeMachine(t).Def
			def.Spec.Gates, def.Spec.Tasks[0].Next = nil, "consume"
			machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			revision := runnerWorkspaceRevision(owner, "web", strings.Repeat("a", 40))
			revision.BaseRepository = &apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
			det := &workspaceRevisionRepoRefDeterministic{revision: revision, received: make(chan apiv1.InvocationEnvelope, 2)}
			r, _ := newParallelTestRunner(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil })
			if owner != "acme" {
				r.cfg.AdditionalRepos = []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: owner, Name: "web", Branch: "source-default"}}
			}
			_, err = r.Start(context.Background(), StartInput{
				RunID: "live-revision", Machine: machine, Gaggle: "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err != nil {
				t.Fatalf("identical re-emission: %v", err)
			}
			if len(det.received) != 2 {
				t.Fatalf("invocations = %d, want 2", len(det.received))
			}
			<-det.received
			env := <-det.received
			if !reflect.DeepEqual(env.WorkspaceRevision, revision) || env.RepoRef.Owner != owner || env.BaseBranch != "main" {
				t.Fatalf("downstream authority: revision=%+v repo=%+v base=%q", env.WorkspaceRevision, env.RepoRef, env.BaseBranch)
			}
		})
	}
}

func TestRunnerWorkspaceRevisionUnsupportedCheckoutRefusesBeforeProvisioning(t *testing.T) {
	revision := runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))
	for _, mode := range []apiv1.WorkspaceMode{"", apiv1.WorkspaceRepo, apiv1.WorkspaceRepoReadOnly, apiv1.WorkspaceScratch} {
		t.Run(string(mode), func(t *testing.T) {
			in := StartInput{workspaceRevision: revision}
			if mode == apiv1.WorkspaceScratch {
				in.pinnedWorkspace = &worktree.Worktree{}
			}
			r := &Runner{}
			_, err := r.createStageWorkspace(context.Background(), in, "inspect", mode, false, "")
			var coded *workspacerevision.Error
			if !errors.As(err, &coded) || coded.Code != workspacerevision.CodeInvalid || !coded.NonRetryable() {
				t.Fatalf("unsupported checkout error = %v", err)
			}
			if got := provisionFailureCode(err); got != coded.Code {
				t.Fatalf("provisioning lost revision code: %s", got)
			}
		})
	}
	r := &Runner{cfg: Config{PinnedWorkspace: true}}
	if _, err := r.acquirePinnedWorkspace(context.Background(), nil, &StartInput{workspaceRevision: revision}); err == nil {
		t.Fatal("selected resume acquired an unsupported pinned checkout")
	}
}

func TestReconstructWorkspaceRevisionRejectsUnacceptedHistory(t *testing.T) {
	machine := workspaceRevisionResumeMachine(t)
	for _, tc := range []struct {
		name, stage, status string
		eventType           journal.EventType
		machine             *workflow.Machine
	}{
		{"failed", "produce", "failure", journal.EventStageFinished, machine},
		{"no-work", "produce", "no-work", journal.EventStageFinished, machine},
		{"agentic", "implement", "success", journal.EventStageFinished, rerunTaskMachine(t)},
		{"unknown-stage", "missing", "success", journal.EventStageFinished, machine},
		{"wrong-event", "produce", "success", journal.EventStageStarted, machine},
		{"missing-workflow", "produce", "success", journal.EventStageFinished, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []journal.Event{{Type: tc.eventType, Stage: tc.stage, Status: tc.status,
				WorkspaceRevision: runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))}}
			if revision, err := reconstructWorkspaceRevision(events, tc.machine); err == nil || revision != nil {
				t.Fatalf("unaccepted history reconstructed: revision=%+v err=%v", revision, err)
			}
		})
	}
}

type workspaceRevisionCapturingDeterministic struct {
	delegate invoke.Deterministic
	received chan apiv1.InvocationEnvelope
}

func (d *workspaceRevisionCapturingDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.received <- env
	return d.delegate.Run(ctx, env, run)
}

func TestRunnerWorkspaceRevisionParallelIsolation(t *testing.T) {
	for _, concurrency := range []int32{1, 2} {
		for _, resume := range []bool{false, true} {
			t.Run(fmt.Sprintf("concurrency=%d/resume=%t", concurrency, resume), func(t *testing.T) {
				const runID = "revision-isolation"
				machine := parallelRunnerMachine(t, concurrency, apiv1.WorkspaceScratch)
				revision := runnerWorkspaceRevision("other", "repo", strings.Repeat("a", 40))
				revision.BaseRepository = &apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
				received := make(chan apiv1.InvocationEnvelope, 4)
				r, runsDir := newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
					return &workspaceRevisionCapturingDeterministic{
						received: received,
						delegate: &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
							runID + ":lens-a": {status: apiv1.ResultSuccess, workspaceRevision: revision},
							runID + ":lens-b": {status: apiv1.ResultSuccess}, runID + ":lens-c": {status: apiv1.ResultSuccess},
							runID + ":collate": {status: apiv1.ResultSuccess},
						}},
					}, nil
				})
				r.cfg.AdditionalRepos = []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "repo", Branch: "main"}}
				base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
				var err error
				if resume {
					createSettledWorkspaceRevisionParallelJournal(t, runsDir, runID, machine, map[int]*apiv1.WorkspaceRevision{1: revision})
					_, err = r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: base})
				} else {
					_, err = r.Start(context.Background(), StartInput{
						RunID: runID, Machine: machine, Gaggle: "demo", RepoRef: base,
						Trigger: journal.Trigger{Kind: journal.TriggerManual},
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				close(received)
				joined := false
				for env := range received {
					if strings.HasSuffix(env.TaskID, ":collate") {
						joined = true
						if !reflect.DeepEqual(env.WorkspaceRevision, revision) || env.RepoRef.Owner != "other" {
							t.Errorf("join lost selection: %+v", env)
						}
					} else if env.WorkspaceRevision != nil || env.RepoRef.Owner != "acme" {
						t.Errorf("branch inherited sibling authority: %+v", env)
					}
				}
				if !joined {
					t.Fatal("join did not execute")
				}
			})
		}
	}
}

func TestRunnerWorkspaceRevisionRerunRestoresAuthority(t *testing.T) {
	const runID = "revision-rerun"
	def := rerunTaskMachine(t).Def
	for i := range def.Spec.Tasks {
		def.Spec.Tasks[i].Workspace = apiv1.WorkspaceScratch
	}
	def.Spec.Start = "produce"
	def.Spec.Tasks = append([]apiv1.Task{{
		Name: "produce", Type: apiv1.TaskDeterministic, Goal: "select", Next: "implement",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
	}}, def.Spec.Tasks...)
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	revision := runnerWorkspaceRevision("other", "repo", strings.Repeat("a", 40))
	revision.BaseRepository = &apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	implementer, finisher := &rerunTaskGoober{}, &capturingSuccessGoober{}
	r, runsDir := newRerunTestRunner(t,
		func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			if name == "implementer" {
				return implementer, nil
			}
			return finisher, nil
		}, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				runID + ":produce": {status: apiv1.ResultSuccess, workspaceRevision: revision},
			}}, nil
		})
	r.cfg.ScratchDir = t.TempDir()
	r.cfg.AdditionalRepos = []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "repo", Branch: "main"}}
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	result, err := r.Start(context.Background(), StartInput{RunID: runID, Machine: machine, Gaggle: "acme-web", RepoRef: base})
	if err != nil || result.Phase != journal.PhaseEscalated {
		t.Fatalf("initial run: result=%+v err=%v", result, err)
	}
	_, err = r.RerunStage(context.Background(), RerunStageInput{
		RunID: runID, Machine: machine, RepoRef: base, Stage: "implement",
		Actor: "operator", InstructionAddendum: "Continue with the recorded selection.",
		ExpectedTerminalSeq: terminalRunSequence(t, runsDir, runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(implementer.invocations) != 2 {
		t.Fatalf("invocations = %d", len(implementer.invocations))
	}
	for _, env := range implementer.invocations {
		if !reflect.DeepEqual(env.WorkspaceRevision, revision) || env.RepoRef.Owner != "other" {
			t.Errorf("live or rerun invocation lost selection: %+v", env)
		}
	}
}

type workspaceRevisionReviewer struct {
	capturingSuccessGoober
	received []apiv1.InvocationEnvelope
}

func (g *workspaceRevisionReviewer) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.received = append(g.received, env)
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestRunnerWorkspaceRevisionReachesReviewer(t *testing.T) {
	def := workspaceRevisionResumeMachine(t).Def
	def.Spec.Gates[0].Evaluator = apiv1.EvaluatorAgentic
	def.Spec.Gates[0].Agentic = &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceScratch}
	def.Spec.Gates[0].Branches["needs-changes"] = workflow.TargetAbort
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	revision := runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))
	reviewer := &workspaceRevisionReviewer{}
	r, _ := newRerunTestRunner(t,
		func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return reviewer, nil },
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return &workspaceRevisionResumeDeterministic{revision: revision, received: make(chan apiv1.InvocationEnvelope, 1)}, nil
		})
	r.cfg.ScratchDir = t.TempDir()
	_, err = r.Start(context.Background(), StartInput{
		RunID: "revision-reviewer", Machine: machine, Gaggle: "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewer.received) != 1 || !reflect.DeepEqual(reviewer.received[0].WorkspaceRevision, revision) {
		t.Fatalf("reviewer lost selection: %+v", reviewer.received)
	}
}

func TestRunnerWorkspaceRevisionResumeUnfinishedBranch(t *testing.T) {
	for _, concurrency := range []int32{1, 2} {
		for _, status := range []apiv1.ResultStatus{apiv1.ResultSuccess, apiv1.ResultFailure} {
			t.Run(fmt.Sprintf("concurrency=%d/status=%s", concurrency, status), func(t *testing.T) {
				const runID = "revision-unfinished"
				def := parallelRunnerMachine(t, concurrency, apiv1.WorkspaceScratch).Def
				def.Spec.Tasks[0].Next = "inspect-a"
				def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{
					Name: "inspect-a", Type: apiv1.TaskDeterministic, Goal: "inspect", Next: workflow.TargetJoin,
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				})
				machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
				if err != nil {
					t.Fatal(err)
				}
				revision := runnerWorkspaceRevision("other", "repo", strings.Repeat("a", 40))
				revision.BaseRepository = &apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
				received := make(chan apiv1.InvocationEnvelope, 4)
				r, runsDir := newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
					return &workspaceRevisionCapturingDeterministic{received: received, delegate: &stubDeterministic{
						rec: rec, byTask: map[string]stubTaskResult{
							runID + ":inspect-a": {status: apiv1.ResultSuccess},
							runID + ":lens-b":    {status: apiv1.ResultSuccess}, runID + ":lens-c": {status: apiv1.ResultSuccess},
							runID + ":collate": {status: apiv1.ResultSuccess},
						},
					}}, nil
				})
				r.cfg.AdditionalRepos = []apiv1.RepoRef{{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "repo"}}
				jr, err := journal.Create(runsDir, journal.RunIdentity{
					RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
					WorkflowDigest: machine.Digest(), Gaggle: "demo", Trigger: journal.Trigger{Kind: journal.TriggerManual},
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = jr.Close() })
				jr.SetMachineState("lens-a")
				for _, event := range []journal.Event{
					{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
						{Branch: 1, Name: "a"}, {Branch: 2, Name: "b"}, {Branch: 3, Name: "c"},
					}},
					{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "a", Stage: "lens-a"},
					{Type: journal.EventStageFinished, Branch: 1, Stage: "lens-a", Attempt: 1, Status: string(status), WorkspaceRevision: revision},
				} {
					if err := jr.Append(event); err != nil {
						t.Fatal(err)
					}
				}
				if err := jr.Close(); err != nil {
					t.Fatal(err)
				}
				_, err = r.Resume(context.Background(), ResumeInput{
					RunID: runID, Machine: machine,
					RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
				})
				if status != apiv1.ResultSuccess {
					if err == nil || len(received) != 0 {
						t.Fatalf("invalid history dispatched: err=%v calls=%d", err, len(received))
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(received) != 4 {
					t.Fatalf("resumed invocations = %d, want 4", len(received))
				}
				close(received)
				for env := range received {
					want := revision
					if strings.HasSuffix(env.TaskID, ":lens-b") || strings.HasSuffix(env.TaskID, ":lens-c") {
						want = nil
					}
					if !reflect.DeepEqual(env.WorkspaceRevision, want) {
						t.Errorf("%s: revision=%+v want=%+v", env.TaskID, env.WorkspaceRevision, want)
					}
				}
			})
		}
	}
}

func TestRunnerWorkspaceRevisionParallelInheritsRoot(t *testing.T) {
	for _, concurrency := range []int32{1, 2} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			const runID = "revision-root"
			def := parallelRunnerMachine(t, concurrency, apiv1.WorkspaceScratch).Def
			def.Spec.Start = "select"
			def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{
				Name: "select", Type: apiv1.TaskDeterministic, Goal: "select", Next: "fan",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			})
			machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			revision := runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))
			received := make(chan apiv1.InvocationEnvelope, 5)
			r, _ := newParallelTestRunner(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
				return &workspaceRevisionCapturingDeterministic{received: received, delegate: &stubDeterministic{
					rec: rec, byTask: map[string]stubTaskResult{
						runID + ":select": {status: apiv1.ResultSuccess, workspaceRevision: revision},
						runID + ":lens-a": {status: apiv1.ResultSuccess}, runID + ":lens-b": {status: apiv1.ResultSuccess},
						runID + ":lens-c": {status: apiv1.ResultSuccess}, runID + ":collate": {status: apiv1.ResultSuccess},
					},
				}}, nil
			})
			_, err = r.Start(context.Background(), StartInput{
				RunID: runID, Machine: machine, Gaggle: "demo",
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(received) != 5 {
				t.Fatalf("invocations = %d, want 5", len(received))
			}
			close(received)
			for env := range received {
				if !strings.HasSuffix(env.TaskID, ":select") && !reflect.DeepEqual(env.WorkspaceRevision, revision) {
					t.Errorf("%s lost pre-parallel authority", env.TaskID)
				}
			}

		})
	}
}

func TestRunnerWorkspaceRevisionRerunCannotForgetLaterSelection(t *testing.T) {
	const runID = "revision-rerun-earlier"
	def := rerunTaskMachine(t).Def
	for i := range def.Spec.Tasks {
		def.Spec.Tasks[i].Workspace = apiv1.WorkspaceScratch
	}
	def.Spec.Tasks[0].Next = "select"
	def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{
		Name: "select", Type: apiv1.TaskDeterministic, Goal: "select", Next: "finish",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
	})
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	revision := runnerWorkspaceRevision("acme", "web", strings.Repeat("a", 40))
	implementer, finisher := &capturingSuccessGoober{}, &rerunTaskGoober{}
	r, runsDir := newRerunTestRunner(t,
		func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			if name == "implementer" {
				return implementer, nil
			}
			return finisher, nil
		}, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{
				runID + ":select": {status: apiv1.ResultSuccess, workspaceRevision: revision},
			}}, nil
		})
	r.cfg.ScratchDir = t.TempDir()
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	started, err := r.Start(context.Background(), StartInput{RunID: runID, Machine: machine, Gaggle: "acme-web", RepoRef: base})
	if err != nil || started.Phase != journal.PhaseEscalated {
		t.Fatalf("initial result=%+v err=%v", started, err)
	}
	_, err = r.RerunStage(context.Background(), RerunStageInput{
		RunID: runID, Machine: machine, RepoRef: base, Stage: "implement", Actor: "operator",
		InstructionAddendum: "Inspect the selected revision.", ExpectedTerminalSeq: terminalRunSequence(t, runsDir, runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(implementer.invocations) != 2 || implementer.invocations[0].WorkspaceRevision != nil ||
		!reflect.DeepEqual(implementer.invocations[1].WorkspaceRevision, revision) {
		t.Fatalf("rerun forgot run-scoped authority: %+v", implementer.invocations)
	}
}
