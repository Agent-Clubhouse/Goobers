package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestTaskWorkspaceModeResolution(t *testing.T) {
	repoRun := &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}
	scratchRun := &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}

	for _, tc := range []struct {
		name string
		task apiv1.Task
		want apiv1.WorkspaceMode
	}{
		{
			name: "unset stays the writable repo worktree",
			task: apiv1.Task{Name: "a", Type: apiv1.TaskAgentic},
			want: apiv1.WorkspaceRepo,
		},
		{
			name: "deterministic run.workspace is honoured",
			task: apiv1.Task{Name: "a", Type: apiv1.TaskDeterministic, Run: scratchRun},
			want: apiv1.WorkspaceScratch,
		},
		{
			name: "agentic task uses the task-level seam",
			task: apiv1.Task{Name: "a", Type: apiv1.TaskAgentic, Workspace: apiv1.WorkspaceRepoReadOnly},
			want: apiv1.WorkspaceRepoReadOnly,
		},
		{
			// Documented precedence: run.workspace wins, so an existing
			// deterministic definition can never change meaning.
			name: "run.workspace outranks the task-level seam",
			task: apiv1.Task{Name: "a", Type: apiv1.TaskDeterministic, Run: repoRun, Workspace: apiv1.WorkspaceScratch},
			want: apiv1.WorkspaceRepo,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskWorkspaceMode(tc.task); got != tc.want {
				t.Errorf("taskWorkspaceMode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGateWorkspaceModeResolution(t *testing.T) {
	plain := apiv1.Gate{Name: "g", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "r"}}
	if got := gateWorkspaceMode(plain); got != apiv1.WorkspaceRepo {
		t.Errorf("unset gate workspace = %q, want the historical repo default", got)
	}
	readOnly := apiv1.Gate{Name: "g", Evaluator: apiv1.EvaluatorAgentic,
		Agentic: &apiv1.AgenticGate{Goober: "r", Workspace: apiv1.WorkspaceRepoReadOnly}}
	if got := gateWorkspaceMode(readOnly); got != apiv1.WorkspaceRepoReadOnly {
		t.Errorf("gate workspace = %q, want repo-readonly", got)
	}
	automated := apiv1.Gate{Name: "g", Evaluator: apiv1.EvaluatorAutomated}
	if got := gateWorkspaceMode(automated); got != apiv1.WorkspaceRepo {
		t.Errorf("automated gate workspace = %q, want the repo default", got)
	}
}

func readOnlyWorkspaceRunner(t *testing.T) (*Runner, StartInput) {
	t.Helper()
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return &countingDeterministic{}, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		ScratchDir:   t.TempDir(),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, StartInput{
		RunID:   "0af7651916cd43dd8448eb211c80319c",
		Gaggle:  "acme-web",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Branch: "main"},
	}
}

// The point of the whole slice: two read-only worktrees for ONE run must
// coexist. A writable repo workspace cannot do this — every one is created on
// the same run branch, and git refuses to check one branch out twice.
func TestReadOnlyWorkspacesCoexistForOneRun(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	ctx := context.Background()

	first, err := r.createStageWorkspace(ctx, in, "lens-a", apiv1.WorkspaceRepoReadOnly, false, "")
	if err != nil {
		t.Fatalf("first read-only workspace: %v", err)
	}
	t.Cleanup(func() { _ = first.Remove(ctx) })

	second, err := r.createStageWorkspace(ctx, in, "lens-b", apiv1.WorkspaceRepoReadOnly, false, "")
	if err != nil {
		t.Fatalf("second read-only workspace must coexist with the first: %v", err)
	}
	t.Cleanup(func() { _ = second.Remove(ctx) })

	if first.path == second.path {
		t.Fatal("the two read-only workspaces share a path")
	}
	for _, ws := range []*stageWorkspace{first, second} {
		if _, err := os.Stat(filepath.Join(ws.path, ".git")); err != nil {
			t.Errorf("read-only workspace %q is not a checkout: %v", ws.path, err)
		}
	}
	// Detached: no branch name, which is precisely why they do not collide.
	if first.worktree != nil && first.worktree.Branch != "" {
		t.Errorf("read-only worktree is on branch %q, want a detached checkout", first.worktree.Branch)
	}
}

// Two WRITABLE repo workspaces for one run collide — the constraint that
// forced the read-only mode into existence. Pinned so the rationale cannot
// quietly stop being true.
func TestWritableRepoWorkspacesCollideForOneRun(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	ctx := context.Background()

	first, err := r.createStageWorkspace(ctx, in, "stage-a", apiv1.WorkspaceRepo, false, "")
	if err != nil {
		t.Fatalf("first writable workspace: %v", err)
	}
	t.Cleanup(func() { _ = first.Remove(ctx) })

	second, err := r.createStageWorkspace(ctx, in, "stage-b", apiv1.WorkspaceRepo, false, "")
	if err == nil {
		t.Cleanup(func() { _ = second.Remove(ctx) })
		t.Fatal("two writable repo workspaces on one run branch should collide; if this now passes, §6.5's rationale needs revisiting")
	}
}

func TestReadOnlyWorkspaceRejectsSyncBaseAndReboundBranch(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	ctx := context.Background()

	if _, err := r.createStageWorkspace(ctx, in, "s", apiv1.WorkspaceRepoReadOnly, true, ""); err == nil {
		t.Error("syncBase must be rejected for a read-only workspace: there is no branch to sync")
	}

	if _, err := r.createStageWorkspace(ctx, in, "s", apiv1.WorkspaceRepoReadOnly, false, "some-branch"); err == nil {
		t.Error("a rebound branch must be rejected for a read-only workspace")
	}
}

func TestPinnedWorkspaceSerializesAllStageModes(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	r.cfg.PinnedWorkspace = true
	r.cfg.PinnedCleanPolicy = "none"
	r.pinnedRuns.Store(in.RunID, &pinnedRunWorkspace{})
	defer r.pinnedRuns.Delete(in.RunID)

	first, err := r.createStageWorkspace(context.Background(), in, "write", apiv1.WorkspaceRepo, false, "")
	if err != nil {
		t.Fatalf("writable pinned workspace: %v", err)
	}
	secondReady := make(chan *stageWorkspace, 1)
	secondErr := make(chan error, 1)
	go func() {
		second, err := r.createStageWorkspace(context.Background(), in, "inspect", apiv1.WorkspaceRepoReadOnly, false, "")
		if err != nil {
			secondErr <- err
			return
		}
		secondReady <- second
	}()
	select {
	case <-secondReady:
		t.Fatal("read-only stage entered the pinned workspace before the writable stage exited")
	case err := <-secondErr:
		t.Fatalf("read-only pinned workspace: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := first.Remove(context.Background()); err != nil {
		t.Fatalf("stage teardown touched pinned workspace: %v", err)
	}
	if _, err := os.Stat(first.path); err != nil {
		t.Fatalf("stage teardown removed pinned workspace: %v", err)
	}
	var second *stageWorkspace
	select {
	case second = <-secondReady:
	case err := <-secondErr:
		t.Fatalf("read-only pinned workspace: %v", err)
	case <-time.After(time.Second):
		t.Fatal("read-only stage did not enter after writable stage exited")
	}
	if first.path != second.path || filepath.Base(first.path) != "pin" {
		t.Fatalf("repo stages used paths %q and %q, want one stable pin", first.path, second.path)
	}
	if err := second.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	scratch, err := r.createStageWorkspace(context.Background(), in, "scratch", apiv1.WorkspaceScratch, false, "")
	if err != nil {
		t.Fatalf("scratch stage was not routed to pinned workspace: %v", err)
	}
	if scratch.path != first.path {
		t.Fatalf("scratch stage path = %q, want pin %q", scratch.path, first.path)
	}
	if err := scratch.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedLeaseSurvivesWatchdogTakeoverUntilOwnerExits(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	r.cfg.PinnedWorkspace = true
	jr, err := journal.Create(r.cfg.RunsDir, journal.RunIdentity{RunID: in.RunID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()

	ownerStarted := make(chan struct{})
	releaseOwner := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		_, err := r.withActiveWorkspaceRun(context.Background(), jr, in.RunID, in.RepoRef, func(context.Context) (Result, error) {
			close(ownerStarted)
			<-releaseOwner
			return Result{}, nil
		})
		returned <- err
	}()
	<-ownerStarted
	active := r.activeRun(in.RunID)
	if active == nil {
		t.Fatal("run owner was not registered")
	}
	if _, claim := active.claimTakeover(); claim != takeoverClaimed {
		t.Fatalf("takeover claim = %v, want claimed", claim)
	}
	active.completeTakeover(activeRunResult{})
	if err := <-returned; err != nil {
		t.Fatalf("takeover return: %v", err)
	}

	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan int, 1)
	acquired := make(chan func() error, 1)
	go func() {
		release, acquireErr := r.cfg.Worktrees.AcquirePinnedLease(context.Background(), repoURL, "next-run", func(position int) {
			queued <- position
		})
		if acquireErr == nil {
			acquired <- release
		}
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("next run did not queue behind taken-over owner")
	}
	select {
	case <-acquired:
		t.Fatal("next run acquired pinned workspace while taken-over owner was still running")
	default:
	}
	close(releaseOwner)
	select {
	case release := <-acquired:
		if err := release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("next run did not acquire after original owner exited")
	}
}
