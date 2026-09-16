//go:build integration

package workerhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func selectedWorkerSource(t *testing.T) (string, string) {
	t.Helper()
	source := t.TempDir()
	runGit(t, source, "init", "--initial-branch=main")
	runGit(t, source, "config", "user.name", "selected-test")
	runGit(t, source, "config", "user.email", "selected@example.test")
	for _, dir := range []string{"included", "excluded"} {
		if err := os.MkdirAll(filepath.Join(source, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, dir, "data.txt"), []byte(dir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "selected")
	runGit(t, source, "config", "uploadpack.allowFilter", "true")
	runGit(t, source, "config", "uploadpack.allowAnySHA1InWant", "true")
	return source, gitOutput(t, source, "rev-parse", "HEAD")
}

func TestIntegrationWorkerSelectedRevisionMaterializationAndRetry(t *testing.T) {
	testdep.Require(t, "git")
	for _, fork := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			for _, sparse := range []bool{false, true} {
				t.Run(fmt.Sprintf("fork=%v/partial=%v/sparse=%v", fork, partial, sparse), func(t *testing.T) {
					t.Parallel()
					ctx := t.Context()
					sourceDir, sha := selectedWorkerSource(t)
					base, revision := workerRevisionFixture()
					source := base
					var additional []apiv1.RepoRef
					if fork {
						source.Owner = "fork"
						revision.Repository.Owner = "fork"
					}
					if sparse {
						source.Checkout = &apiv1.CheckoutSpec{Sparse: []string{"included"}}
					}
					if fork {
						additional = []apiv1.RepoRef{source}
					} else {
						base = source
					}
					revision.CommitSHA = sha
					revision.SourceRef = "main"
					req := engine.WorkspaceRequest{RunID: "selected", Stage: "inspect", RepoRef: base,
						Mode: apiv1.WorkspaceRepoReadOnly, WorkspaceRevision: revision,
						Checkout: &apiv1.CheckoutSpec{Sparse: []string{"excluded"}}}
					for attempt := 0; attempt < 2; attempt++ {
						options := []worktree.ManagerOption{}
						if partial {
							options = append(options, worktree.WithPartialClone())
						}
						manager, err := worktree.NewManager(filepath.Join(t.TempDir(), "worker"), options...)
						if err != nil {
							t.Fatal(err)
						}
						p := &WorktreeWorkspaces{Manager: manager, ConfiguredBase: base, AdditionalRepos: additional,
							CloneURL: func(ref apiv1.RepoRef) (string, error) {
								if !reflect.DeepEqual(ref, source) {
									t.Errorf("routing/policy = %+v, want configured source %+v", ref, source)
								}
								return sourceDir, nil
							}}
						ws, err := p.Provision(ctx, req)
						if err != nil {
							t.Fatal(err)
						}
						if got := gitOutput(t, ws.Path(), "rev-parse", "HEAD"); got != sha {
							t.Fatalf("worker %d HEAD = %s, want %s", attempt, got, sha)
						}
						if got := gitOutput(t, ws.Path(), "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
							t.Fatalf("selected checkout is not detached: %s", got)
						}
						if _, err := os.Stat(filepath.Join(ws.Path(), "included", "data.txt")); err != nil {
							t.Fatal(err)
						}
						_, statErr := os.Stat(filepath.Join(ws.Path(), "excluded", "data.txt"))
						if sparse != os.IsNotExist(statErr) {
							t.Fatalf("sparse=%v, excluded file: %v", sparse, statErr)
						}
						if partial && gitOutput(t, ws.Path(), "config", "--get", "remote.origin.promisor") != "true" {
							t.Fatal("partial clone setting lost")
						}
						pub, err := ws.(engine.DeltaPublisher).PublishDelta(ctx)
						if err != nil || pub != (engine.WorkspaceDeltaPublication{}) {
							t.Fatalf("readonly published delta: %+v, %v", pub, err)
						}
						if err := ws.Remove(ctx); err != nil {
							t.Fatal(err)
						}
						if attempt == 0 {
							runGit(t, sourceDir, "commit", "--allow-empty", "-m", "source branch moved")
						}
					}
				})
			}
		}
	}
}

func TestIntegrationWorkerSelectedRevisionFailureParity(t *testing.T) {
	testdep.Require(t, "git")
	source, _ := selectedWorkerSource(t)
	for _, tc := range []struct{ name, object, code string }{
		{"missing", strings.Repeat("b", 40), workspacerevision.CodeAcquisition},
		{"tree", gitOutput(t, source, "rev-parse", "HEAD^{tree}"), workspacerevision.CodeObjectType},
		{"blob", gitOutput(t, source, "rev-parse", "HEAD:included/data.txt"), workspacerevision.CodeObjectType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, revision := workerRevisionFixture()
			revision.CommitSHA = tc.object
			manager, err := worktree.NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := &WorktreeWorkspaces{Manager: manager, ConfiguredBase: base,
				CloneURL: func(apiv1.RepoRef) (string, error) { return source, nil }}
			_, err = p.Provision(context.Background(), engine.WorkspaceRequest{RunID: "failure", Stage: "inspect",
				RepoRef: base, Mode: apiv1.WorkspaceRepoReadOnly, WorkspaceRevision: revision})
			var typed *workspacerevision.Error
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
		})
	}
}
