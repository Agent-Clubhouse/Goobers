package workerhost

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/internal/worktree"
)

func ownedWorkerRequest(t *testing.T, mode apiv1.WorkspaceMode, revision *apiv1.WorkspaceRevision) engine.WorkspaceRequest {
	t.Helper()
	base, _ := workerRevisionFixture()
	req := engine.WorkspaceRequest{
		RunID: "owned", Stage: "author", Workflow: "sandbox", BranchNamespace: "goobers/",
		Mode: mode, RepoRef: base, WorkspaceRevision: revision,
	}
	binding, err := workspacebranch.Expected(base, revision, req.BranchNamespace, req.Workflow, req.RunID)
	if err != nil {
		t.Fatal(err)
	}
	req.WorkspaceBranchBinding = binding
	req.WorkspaceBranch = strings.TrimPrefix(binding.Ref, "refs/heads/")
	return req
}

func TestOwnedWorkspaceRefusesInvalidRequestsBeforeGit(t *testing.T) {
	for _, mode := range []apiv1.WorkspaceMode{"", apiv1.WorkspaceRepo} {
		for _, tc := range []struct {
			name string
			edit func(*engine.WorkspaceRequest)
		}{
			{"branch", func(req *engine.WorkspaceRequest) { req.WorkspaceBranch = "goobers/another" }},
			{"sync", func(req *engine.WorkspaceRequest) { req.SyncBase = true }},
			{"ownership", func(req *engine.WorkspaceRequest) { req.RunID = "another-run" }},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				base, revision := workerRevisionFixture()
				req := ownedWorkerRequest(t, mode, revision)
				tc.edit(&req)
				p := &WorktreeWorkspaces{ConfiguredBase: base}
				ws, err := p.Provision(t.Context(), req)
				var typed *workspacerevision.Error
				if ws != nil || !errors.As(err, &typed) || !typed.NonRetryable() {
					t.Fatalf("workspace=%v, error=%v; want typed refusal before manager access", ws, err)
				}
			})
		}
	}
}

func TestOwnedWorkspaceUsesConfiguredBase(t *testing.T) {
	for _, mode := range []apiv1.WorkspaceMode{"", apiv1.WorkspaceRepo} {
		t.Run(string(mode), func(t *testing.T) {
			repo := newFixtureRepo(t)
			base, revision := workerRevisionFixture()
			revision.CommitSHA = gitOutput(t, repo, "--git-dir="+repo, "rev-parse", "HEAD")
			req := ownedWorkerRequest(t, mode, revision)
			runGit(t, repo, "--git-dir="+repo, "update-ref", req.WorkspaceBranchBinding.Ref, revision.CommitSHA)
			req.RepoRef.Owner = "untrusted-request"
			manager, err := worktree.NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := &WorktreeWorkspaces{
				Manager: manager, ConfiguredBase: base,
				CloneURL: func(ref apiv1.RepoRef) (string, error) {
					if !reflect.DeepEqual(ref, base) {
						t.Errorf("clone target = %+v, want configured base %+v", ref, base)
					}
					return repo, nil
				},
			}
			ws, err := p.Provision(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := ws.Remove(t.Context()); err != nil {
					t.Error(err)
				}
			}()
			if err := worktree.VerifyOwnedWorkspace(t.Context(), ws.Path(), *req.WorkspaceBranchBinding); err != nil {
				t.Fatal(err)
			}
		})
	}
}
