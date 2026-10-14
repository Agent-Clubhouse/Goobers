//go:build integration

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Exercise a compiler-accepted writable graph through the actual dispatcher,
// branch executors and durable fan-in. Public start remains separately gated.
func TestIntegrationCompiledParallelForkJoin(t *testing.T) {
	testdep.Require(t, "git")
	t.Run("one-slot", func(t *testing.T) { verifyCompiledForkJoin(t, 1, nil) })
	t.Run("two-slots", func(t *testing.T) { verifyCompiledForkJoin(t, 2, nil) })
}

func verifyCompiledForkJoin(t *testing.T, width int32, waiting *compiledChildWait) {
	t.Helper()
	fork := prepareChildWorkspaceFixture(t, false)
	machine := compiledForkMachine(t, width)
	run, err := journal.Create(fork.config.RunsDir, journal.RunIdentity{RunID: "compiled-parent", Workflow: machine.Def.Name, Gaggle: "web", WorkflowDigest: machine.Digest()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	in := StartInput{RunID: "compiled-parent", Gaggle: "web", Machine: machine, RepoRef: fork.input.RepoRef}
	cfg := fork.config
	cfg.ChildHandoff = noChildRequest{}
	cfg.ChildParentCapacity = noChildRequest{}
	if waiting != nil {
		cfg.ChildHandoff, cfg.ChildParentCapacity = waiting, waiting
	}
	service := parallelworkspace.Service{Worktrees: cfg.Worktrees, CloneURL: cfg.RepoCloneURL, Policy: func(string) (recovery.SnapshotPolicy, error) { return recovery.SnapshotPolicy{}, nil }}
	cfg.PrepareParentForkSource = func(ctx context.Context, rec OwnedJournalRecorder, req spec.Request, previous *spec.Source) (spec.Source, error) {
		return service.Prepare(ctx, rec, req, previous)
	}
	cfg.PrepareParentForkResult = func(ctx context.Context, rec OwnedJournalRecorder, req spec.ResultRequest, previous *spec.Source) (spec.Source, error) {
		return service.Result(ctx, rec, req, previous)
	}
	cfg.JoinParentFork = func(ctx context.Context, rec OwnedJournalRecorder, req spec.JoinRequest) error {
		return service.Join(ctx, rec, req)
	}
	paths := map[string]string{}
	var pathsMu sync.Mutex
	cfg.NewAgentic = func(_ string, rec ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
		base := childWorkspaceAgent{invoke: func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			name := strings.TrimPrefix(env.TaskID, in.RunID+":")
			pathsMu.Lock()
			paths[name] = env.Workspace
			pathsMu.Unlock()
			switch name {
			case "a", "b":
				other := "a"
				if name == "a" {
					other = "b"
				}
				if _, err := os.Stat(filepath.Join(env.Workspace, other+".txt")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("branch %s observed sibling edits: %v", name, err)
				}
				childWorkspaceWrite(t, env.Workspace, name+".txt", []byte(name+" result\n"))
				runGit(t, env.Workspace, "add", name+".txt")
				runGit(t, env.Workspace, "-c", "user.name=Fork Test", "-c", "user.email=fork-test@example.invalid", "commit", "-m", name)
			case "join":
				if waiting != nil {
					childWorkspaceRead(t, env.Workspace, "pending.txt", []byte("retained branch edits"))
				}
				childWorkspaceRead(t, env.Workspace, "a.txt", []byte("a result\n"))
				childWorkspaceRead(t, env.Workspace, "b.txt", []byte("b result\n"))
			default:
				t.Errorf("unexpected task %s", name)
			}
			if err := recordCompiledParentReturn(rec, env); err != nil {
				return apiv1.ResultEnvelope{}, err
			}

			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"summary": name + " complete"}}, nil
		}}
		if waiting != nil {
			return &compiledWaitingAgent{base: base, waiting: waiting, recorder: rec}, nil
		}
		return base, nil
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parallel, _ := machine.Parallel("fan")
	if err := validateConcurrentParallelWorkspaces(machine, parallel); err != nil {
		t.Fatal(err)
	}
	registrar, _ := journal.DefaultScrubber()
	joined, err := r.runConcurrentParallel(t.Context(), run, in, parallel, nil, nil, "", apiv1.ResultEnvelope{}, nil, "", registrar, &atomic.Int64{})
	if err != nil || !joined.runJoin || joined.target != "join" {
		t.Fatalf("parallel result=%+v error=%v", joined, err)
	}
	task, _ := machine.Task("join")
	frame := taskFrame{jr: run, in: in, t: task, ex: childOriginExecutors(cfg, run), branchRecorded: new(bool), reboundRecorded: new(string)}
	if _, _, err := r.runTask(t.Context(), frame, 0, 1, "", "", nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || paths["a"] == paths["b"] || paths["join"] == paths["a"] || paths["join"] == paths["b"] {
		t.Fatalf("branch/root workspace isolation lost: %v", paths)
	}
	if waiting != nil {
		waiting.verify(t, run)
	}
	fork.assertParentUnchanged(t)
}

func compiledForkMachine(t *testing.T, width int32) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{Name: "compiled-forks", Version: 1, DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Start: "fan", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Tasks: []apiv1.Task{
			{Name: "a", Type: apiv1.TaskAgentic, Goal: "produce a", Goober: "coder", Workspace: apiv1.WorkspaceRepo, ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}, Next: workflow.TargetJoin},
			{Name: "b", Type: apiv1.TaskAgentic, Goal: "produce b", Goober: "coder", Workspace: apiv1.WorkspaceRepo, Next: workflow.TargetJoin},
			{Name: "join", Type: apiv1.TaskAgentic, Goal: "inspect contributions", Goober: "coder", Workspace: apiv1.WorkspaceRepo, RepoFrom: apiv1.RepoFrom{"a", "b"}},
		},
		Parallels: []apiv1.Parallel{{Name: "fan", MaxConcurrentBranches: width, Join: "join", FailurePolicy: apiv1.BranchContinueOnError, Branches: []apiv1.Branch{{Name: "left", Start: "a"}, {Name: "right", Start: "b"}}}},
	}}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

// The stage opts in but chooses to finish without requesting a child.
type noChildRequest struct{}

func (noChildRequest) Await(ctx context.Context, _ apiv1.InvocationEnvelope) (ChildHandoffRequest, error) {
	<-ctx.Done()
	return ChildHandoffRequest{}, ctx.Err()
}
func (noChildRequest) Yield(context.Context, ChildHandoffRequest, ChildWorkspaceCustody) error {
	return errors.New("unexpected child yield")
}
func (noChildRequest) Wait(context.Context, ChildHandoffRequest) (ChildHandoffCompletion, error) {
	return ChildHandoffCompletion{}, errors.New("unexpected child wait")
}
func (noChildRequest) SuspendChildParent(context.Context, string) (ChildParentSuspension, error) {
	return nil, errors.New("unexpected child suspension")
}

func recordCompiledParentReturn(rec ArtifactRecorder, env apiv1.InvocationEnvelope) error {
	if env.ChildWorkflowOrigin == nil {
		return nil
	}
	// The synchronous host fixture supplies the verified-return receipt
	// normally written by the isolated worker factory after its writer stops.
	owned, _, err := OwnedJournalScope(rec)
	if err != nil {
		return err
	}
	output, err := owned.RecordArtifact("fixture-return.json", []byte("verified synchronous return"))
	if err != nil {
		return err
	}
	return RecordParentContribution(owned, env, journal.Digest([]byte("fixture-contract:"+env.ChildWorkflowOrigin.AttemptID)), output)
}
