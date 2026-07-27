package workerhost

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
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
	cmd := exec.Command("git", args...)
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
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
