package runner

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/worktree"
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

func TestPinnedWorkspaceBacksEveryStageWithoutWorktrees(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := r.cfg.Worktrees.AcquirePinned(context.Background(), worktree.PinnedOptions{
		RepoURL: repoURL, RunID: in.RunID, BaseRef: "main", Branch: "goobers/test/" + in.RunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	in.pinnedWorkspace = lease.Worktree
	in.pinnedStage = &sync.Mutex{}

	var paths []string
	for _, mode := range []apiv1.WorkspaceMode{apiv1.WorkspaceScratch, apiv1.WorkspaceRepo, apiv1.WorkspaceRepoReadOnly} {
		workspace, err := r.createStageWorkspace(context.Background(), in, string(mode), mode, false, "")
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, workspace.path)
		if err := workspace.Remove(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if paths[0] != lease.Worktree.Path || paths[1] != lease.Worktree.Path || paths[2] != lease.Worktree.Path {
		t.Fatalf("stage paths = %v, want shared pin %q", paths, lease.Worktree.Path)
	}
	runDirs, err := filepath.Glob(filepath.Join(r.cfg.Worktrees.Root, "*", "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runDirs) != 0 {
		t.Fatalf("pinned stages created per-run worktree directories: %v", runDirs)
	}
}

func TestPinnedWorkspaceHonorsReboundBranchAndSyncBase(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	updater := filepath.Join(t.TempDir(), "updater")
	runGit(t, "", "clone", repoURL, updater)
	runGit(t, updater, "config", "user.name", "test")
	runGit(t, updater, "config", "user.email", "test@example.com")
	runGit(t, updater, "checkout", "-b", "goobers/remediation/pr")
	if err := os.WriteFile(filepath.Join(updater, "pr-marker.txt"), []byte("pr"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, updater, "add", "pr-marker.txt")
	runGit(t, updater, "commit", "-m", "add PR marker")
	runGit(t, updater, "push", "origin", "goobers/remediation/pr")

	lease, err := r.cfg.Worktrees.AcquirePinned(context.Background(), worktree.PinnedOptions{
		RepoURL: repoURL, RunID: in.RunID, BaseRef: "main", Branch: "goobers/test/" + in.RunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	in.pinnedWorkspace = lease.Worktree
	in.pinnedStage = &sync.Mutex{}

	baseline, err := r.createStageWorkspace(context.Background(), in, "implement", apiv1.WorkspaceRepo, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}

	rebound, err := r.createStageWorkspace(context.Background(), in, "remediate", apiv1.WorkspaceRepo, false, "goobers/remediation/pr")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(rebound.path, "pr-marker.txt")); err != nil || string(got) != "pr" {
		t.Fatalf("rebound marker = %q, %v", got, err)
	}
	if err := rebound.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}

	readOnly, err := r.createStageWorkspace(context.Background(), in, "inspect", apiv1.WorkspaceRepoReadOnly, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(readOnly.path, "pr-marker.txt")); !os.IsNotExist(err) {
		t.Fatalf("read-only stage remained on rebound branch: %v", err)
	}
	if err := readOnly.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}

	runGit(t, updater, "checkout", "main")
	if err := os.WriteFile(filepath.Join(updater, "base-update.txt"), []byte("latest"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, updater, "add", "base-update.txt")
	runGit(t, updater, "commit", "-m", "advance base")
	runGit(t, updater, "push", "origin", "main")

	synced, err := r.createStageWorkspace(context.Background(), in, "local-ci", apiv1.WorkspaceRepo, true, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = synced.Remove(context.Background()) }()
	if got, err := os.ReadFile(filepath.Join(synced.path, "base-update.txt")); err != nil || string(got) != "latest" {
		t.Fatalf("synced base file = %q, %v", got, err)
	}
}
