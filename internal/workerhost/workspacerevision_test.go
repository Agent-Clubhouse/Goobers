package workerhost

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func workerRevisionFixture() (apiv1.RepoRef, *apiv1.WorkspaceRevision) {
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	return base, &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: base.Provider, Owner: base.Owner, Name: base.Name},
		CommitSHA:  strings.Repeat("a", 40),
	}
}

func TestSelectedRevisionRefusesInvalidRequestsBeforeGit(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*engine.WorkspaceRequest)
		code string
	}{
		{"branch", func(r *engine.WorkspaceRequest) { r.WorkspaceBranch = "goobers/source" }, workspacerevision.CodeInvalid},
		{"delta", func(r *engine.WorkspaceRequest) { r.WorkspaceDelta = "sha256:bundle" }, workspacerevision.CodeInvalid},
		{"sync", func(r *engine.WorkspaceRequest) { r.SyncBase = true }, workspacerevision.CodeInvalid},
		{"sha", func(r *engine.WorkspaceRequest) { r.WorkspaceRevision.CommitSHA = "main" }, workspacerevision.CodeInvalid},
		{"source", func(r *engine.WorkspaceRequest) { r.WorkspaceRevision.Repository.Owner = "unconfigured" }, workspacerevision.CodeUnauthorized},
		{"host", func(r *engine.WorkspaceRequest) {
			r.WorkspaceRevision.Repository.URL = "https://other.example/acme/web"
		}, workspacerevision.CodeUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, revision := workerRevisionFixture()
			req := engine.WorkspaceRequest{RunID: "selected", Stage: "inspect", RepoRef: base,
				Mode: apiv1.WorkspaceRepoReadOnly, WorkspaceRevision: revision}
			tc.edit(&req)
			p := &WorktreeWorkspaces{ConfiguredBase: base}
			_, err := p.Provision(context.Background(), req)
			var typed *workspacerevision.Error
			if !errors.As(err, &typed) || typed.Code != tc.code || !typed.NonRetryable() {
				t.Fatalf("error = %v, want nonretryable %s before manager access", err, tc.code)
			}
		})
	}
}

func TestSelectedRevisionScratchDoesNotAcquireSource(t *testing.T) {
	base, revision := workerRevisionFixture()
	p := &WorktreeWorkspaces{ScratchDir: t.TempDir()}
	ws, err := p.Provision(context.Background(), engine.WorkspaceRequest{
		RunID: "selected", Stage: "selector", RepoRef: base, Mode: apiv1.WorkspaceScratch,
		WorkspaceRevision: revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}
