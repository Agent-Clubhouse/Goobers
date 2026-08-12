package workerhost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func TestWorktreeWorkspacesScratchMode(t *testing.T) {
	p := &WorktreeWorkspaces{ScratchDir: filepath.Join(t.TempDir(), "scratch")}
	ws, err := p.Provision(context.Background(), engine.WorkspaceRequest{
		RunID: "run-1", Stage: "build", Mode: apiv1.WorkspaceScratch,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if ws.Path() == "" {
		t.Fatal("scratch workspace has no path")
	}
	if _, err := os.Stat(ws.Path()); err != nil {
		t.Fatalf("scratch workspace missing on disk: %v", err)
	}
	if err := ws.Remove(context.Background()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(ws.Path()); !os.IsNotExist(err) {
		t.Fatalf("scratch workspace still present after Remove: %v", err)
	}
}

func TestWorktreeWorkspacesRepoMode(t *testing.T) {
	repo := newFixtureRepo(t)
	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	p := &WorktreeWorkspaces{
		Manager:  mgr,
		CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
	}
	ws, err := p.Provision(context.Background(), engine.WorkspaceRequest{
		RunID:    "run-2",
		Stage:    "implement",
		Gaggle:   "web",
		Workflow: "implementation",
		RepoRef:  apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:     apiv1.WorkspaceRepo,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() { _ = ws.Remove(context.Background()) }()
	if _, err := os.Stat(filepath.Join(ws.Path(), "README.md")); err != nil {
		t.Fatalf("repo workspace missing checkout content: %v", err)
	}
	// The checked-out branch is the run branch, derived exactly as the local
	// runner derives it (default namespace, workflow, run id).
	head := gitOutput(t, ws.Path(), "rev-parse", "--abbrev-ref", "HEAD")
	if head != "goobers/implementation/run-2" {
		t.Fatalf("checked-out branch = %q, want the run branch", head)
	}
}

func TestWorktreeWorkspacesSelectedBranch(t *testing.T) {
	const selected = "goobers/implementation/pr-head"
	repo := newFixtureRepo(t)
	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := mgr.WorkingCopy(context.Background(), repo); err != nil {
		t.Fatalf("prewarm stale mirror: %v", err)
	}
	seed := t.TempDir()
	runGit(t, "", "clone", repo, seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "test")
	runGit(t, seed, "checkout", "-b", selected)
	if err := os.WriteFile(filepath.Join(seed, "selected.txt"), []byte("selected revision\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "selected.txt")
	runGit(t, seed, "commit", "-m", "selected branch change")
	selectedRevision := gitOutput(t, seed, "rev-parse", "HEAD")
	runGit(t, seed, "push", "origin", selected)

	p := &WorktreeWorkspaces{
		Manager:  mgr,
		CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
	}
	req := engine.WorkspaceRequest{
		RunID: "selected-run", Stage: "select", Workflow: "remediation",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:    apiv1.WorkspaceRepo,
	}
	req.Stage = "rework"
	req.WorkspaceBranch = selected
	rebound, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("rebound Provision: %v", err)
	}
	if head := gitOutput(t, rebound.Path(), "rev-parse", "--abbrev-ref", "HEAD"); head != selected {
		t.Errorf("checked-out branch = %q, want %q", head, selected)
	}
	if revision := gitOutput(t, rebound.Path(), "rev-parse", "HEAD"); revision != selectedRevision {
		t.Errorf("engine workspace revision = %q, want selected revision %q", revision, selectedRevision)
	}
	if _, err := os.Stat(filepath.Join(rebound.Path(), "selected.txt")); err != nil {
		t.Fatalf("selected branch change is missing: %v", err)
	}
	if err := rebound.Remove(context.Background()); err != nil {
		t.Fatalf("remove rebound workspace: %v", err)
	}

	retryManager, err := worktree.NewManager(mgr.Root)
	if err != nil {
		t.Fatalf("new retry manager: %v", err)
	}
	p.Manager = retryManager
	retried, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("retry Provision: %v", err)
	}
	if revision := gitOutput(t, retried.Path(), "rev-parse", "HEAD"); revision != selectedRevision {
		t.Errorf("retried workspace revision = %q, want selected revision %q", revision, selectedRevision)
	}
	if err := retried.Remove(context.Background()); err != nil {
		t.Fatalf("remove retried workspace: %v", err)
	}

	req.Stage = "verify"
	req.WorkspaceBranch = "goobers/implementation/missing"
	if _, err := p.Provision(context.Background(), req); err == nil {
		t.Fatal("missing selected branch was silently created from the default branch")
	}
	req.WorkspaceBranch = "main"
	if _, err := p.Provision(context.Background(), req); err == nil {
		t.Fatal("selected branch outside the run namespace fell back to the default branch")
	}
}

type revisionObservingDeterministic struct {
	mu        sync.Mutex
	t         *testing.T
	selected  string
	revisions map[string]string
}

func (d *revisionObservingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	stage := strings.TrimPrefix(env.TaskID, env.RunID+":")
	revision := gitOutput(d.t, env.Workspace, "rev-parse", "HEAD")
	d.mu.Lock()
	d.revisions[stage] = revision
	d.mu.Unlock()
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
	if stage == "select" {
		result.Outputs = map[string]interface{}{runner.WorkspaceBranchOutput: d.selected}
	}
	return result, nil
}

func (d *revisionObservingDeterministic) revision(stage string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.revisions[stage]
}

func TestSelectedBranchRevisionMatchesLocalRunner(t *testing.T) {
	const selected = "goobers/implementation/parity"
	repo := newFixtureRepo(t)
	seed := t.TempDir()
	runGit(t, "", "clone", repo, seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "test")
	runGit(t, seed, "checkout", "-b", selected)
	if err := os.WriteFile(filepath.Join(seed, "selected.txt"), []byte("selected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "selected.txt")
	runGit(t, seed, "commit", "-m", "selected revision")
	selectedRevision := gitOutput(t, seed, "rev-parse", "HEAD")
	runGit(t, seed, "push", "origin", selected)

	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "select",
		Tasks: []apiv1.Task{
			{Name: "select", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "verify"},
			{Name: "verify", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "branch-parity", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile fixture: %v", err)
	}

	localObserver := &revisionObservingDeterministic{t: t, selected: selected, revisions: map[string]string{}}
	localRoot := t.TempDir()
	localManager, err := worktree.NewManager(filepath.Join(localRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new local manager: %v", err)
	}
	localRunner, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return localObserver, nil
		},
		Worktrees: localManager,
		RunsDir:   filepath.Join(localRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return repo, nil
		},
	})
	if err != nil {
		t.Fatalf("new local runner: %v", err)
	}
	if _, err := localRunner.Start(context.Background(), runner.StartInput{
		RunID: "local-parity", Machine: machine, Gaggle: "web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}); err != nil {
		t.Fatalf("local runner: %v", err)
	}

	engineObserver := &revisionObservingDeterministic{t: t, selected: selected, revisions: map[string]string{}}
	engineManager, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatalf("new engine manager: %v", err)
	}
	workspaces := &WorktreeWorkspaces{
		Manager:  engineManager,
		CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
	}
	preview := true
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&engine.Activities{Det: engineObserver, Workspaces: workspaces})
	env.ExecuteWorkflow(engine.Run, engine.RunInput{
		RunID: "engine-parity", Gaggle: "web", WorkflowName: "branch-parity", Version: 1,
		PreviewFeaturesEnabled: &preview,
		Spec:                   spec,
		RepoRef:                apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("engine runner: %v", err)
	}

	if localRevision, engineRevision := localObserver.revision("verify"), engineObserver.revision("verify"); localRevision != selectedRevision || engineRevision != selectedRevision {
		t.Errorf("selected revision: local=%q engine=%q, want both %q", localRevision, engineRevision, selectedRevision)
	}
}

// TestWorktreeWorkspacesRepoModeSyncBase: a SyncBase request reaches
// worktree.CreateOptions, so a run.syncBase stage (#813) executes against a
// run branch carrying the freshly fetched base — the same threading the local
// runner's createStageWorkspace applies.
func TestWorktreeWorkspacesRepoModeSyncBase(t *testing.T) {
	src := t.TempDir()
	runGit(t, src, "init", "--initial-branch=main")
	runGit(t, src, "config", "user.email", "test@example.com")
	runGit(t, src, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "README.md")
	runGit(t, src, "commit", "-m", "initial")

	mgr, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p := &WorktreeWorkspaces{
		Manager:  mgr,
		CloneURL: func(apiv1.RepoRef) (string, error) { return src, nil },
	}
	req := engine.WorkspaceRequest{
		RunID:    "run-3",
		Stage:    "implement",
		Gaggle:   "web",
		Workflow: "implementation",
		RepoRef:  apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:     apiv1.WorkspaceRepo,
	}
	first, err := p.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.Path(), "implementation.txt"), []byte("run change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, first.Path(), "add", "implementation.txt")
	runGit(t, first.Path(), "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", "implement")
	if err := first.Remove(context.Background()); err != nil {
		t.Fatalf("remove first workspace: %v", err)
	}

	// Base advances in the origin between the stages.
	if err := os.WriteFile(filepath.Join(src, "build-fix.txt"), []byte("latest build behavior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "build-fix.txt")
	runGit(t, src, "commit", "-m", "fix build behavior")

	syncReq := req
	syncReq.Stage = "local-ci"
	syncReq.SyncBase = true
	synced, err := p.Provision(context.Background(), syncReq)
	if err != nil {
		t.Fatalf("synced Provision: %v", err)
	}
	defer func() { _ = synced.Remove(context.Background()) }()
	for _, name := range []string{"implementation.txt", "build-fix.txt"} {
		if _, err := os.Stat(filepath.Join(synced.Path(), name)); err != nil {
			t.Fatalf("synced workspace missing %s: %v", name, err)
		}
	}
}

func TestWorktreeWorkspacesFailsClosed(t *testing.T) {
	p := &WorktreeWorkspaces{}
	if _, err := p.Provision(context.Background(), engine.WorkspaceRequest{Stage: "s", Mode: apiv1.WorkspaceScratch}); err == nil {
		t.Error("scratch mode without a scratch dir must fail")
	}
	if _, err := p.Provision(context.Background(), engine.WorkspaceRequest{Stage: "s", Mode: apiv1.WorkspaceRepo}); err == nil {
		t.Error("repo mode without a manager must fail")
	}
	if _, err := p.Provision(context.Background(), engine.WorkspaceRequest{Stage: "s", Mode: "warp"}); err == nil {
		t.Error("unknown mode must fail")
	}
	scratch := &WorktreeWorkspaces{ScratchDir: t.TempDir()}
	if _, err := scratch.Provision(context.Background(), engine.WorkspaceRequest{Stage: "s", Mode: apiv1.WorkspaceScratch, SyncBase: true}); err == nil || !strings.Contains(err.Error(), "syncBase requires a repo workspace") {
		t.Errorf("scratch + syncBase = %v, want the repo-workspace refusal", err)
	}
}

func newFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runGit(t, work, "init", "--initial-branch=main")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testgit.Command(args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
