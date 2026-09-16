//go:build integration

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func remoteBranchFixture(t *testing.T, fork bool) (RemoteBranchOptions, string) {
	t.Helper()
	source := newSourceRepo(t)
	target := filepath.Join(t.TempDir(), "target.git")
	runTestGit(t, "", "clone", "--bare", source, target)
	if fork {
		mustWriteFile(t, filepath.Join(source, "fork-only.txt"), "exact fork object\n")
		runTestGit(t, source, "add", ".")
		runTestGit(t, source, "commit", "-m", "fork-only")
	}
	sha := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	sourceRemote := target
	if fork {
		sourceRemote = filepath.Join(t.TempDir(), "source.git")
		runTestGit(t, "", "clone", "--bare", source, sourceRemote)
	}
	return RemoteBranchOptions{
		SourceURL: sourceRemote, TargetURL: target, TempDir: t.TempDir(),
		Binding: apiv1.WorkspaceBranchBinding{
			Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "base"},
			Ref:        "refs/heads/custom/workflow/run", StartingSHA: sha,
		},
		SourceRead:  RemoteBranchAccess{Authorized: true},
		TargetWrite: RemoteBranchAccess{Authorized: true},
	}, source
}

func requireBranchCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *workspacerevision.Error
	if !errors.As(err, &failure) || failure.Code != code || failure.NonRetryable() != (code != workspacerevision.CodeAcquisition) {
		t.Fatalf("error = %v, want %s with stable retryability", err, code)
	}
}

func TestIntegrationRemoteBranchEstablishmentAndPublication(t *testing.T) {
	ctx := context.Background()
	for _, fork := range []bool{false, true} {
		name := "same-repository"
		if fork {
			name = "fork"
		}
		t.Run(name, func(t *testing.T) {
			opts, _ := remoteBranchFixture(t, fork)
			before := runTestGit(t, opts.SourceURL, "show-ref", "--heads")
			if err := EstablishRemoteBranch(ctx, opts); err != nil {
				t.Fatal(err)
			}
			// Simulate a lost response/crash before journaling, with no local
			// transfer directory surviving and the source unavailable.
			reconcile := opts
			reconcile.SourceURL = filepath.Join(t.TempDir(), "missing-source.git")
			if err := EstablishRemoteBranch(ctx, reconcile); err != nil {
				t.Fatalf("crash reconciliation: %v", err)
			}
			actual := strings.TrimSpace(runTestGit(t, opts.TargetURL, "rev-parse", opts.Binding.Ref))
			if actual != opts.Binding.StartingSHA {
				t.Fatalf("target = %s", actual)
			}
			workspace := filepath.Join(t.TempDir(), "author")
			branch := strings.TrimPrefix(opts.Binding.Ref, "refs/heads/")
			runTestGit(t, "", "clone", "--branch", branch, opts.TargetURL, workspace)
			runTestGit(t, workspace, "config", "user.name", "fixture")
			runTestGit(t, workspace, "config", "user.email", "fixture@example.invalid")
			mustWriteFile(t, filepath.Join(workspace, "authored.txt"), "owned branch only")
			runTestGit(t, workspace, "add", ".")
			runTestGit(t, workspace, "commit", "-m", "author")
			tip := strings.TrimSpace(runTestGit(t, workspace, "rev-parse", "HEAD"))
			// Malicious stage routing/config cannot redirect the broker.
			runTestGit(t, workspace, "remote", "set-url", "origin", opts.SourceURL)
			runTestGit(t, workspace, "config", "remote.origin.push", "HEAD:refs/heads/main")
			if err := PublishRemoteBranch(ctx, opts, workspace); err != nil {
				t.Fatal(err)
			}
			if err := PublishRemoteBranch(ctx, opts, workspace); err != nil {
				t.Fatalf("publication rediscovery: %v", err)
			}
			if actual := strings.TrimSpace(runTestGit(t, opts.TargetURL, "rev-parse", opts.Binding.Ref)); actual != tip {
				t.Fatalf("published %s, want %s", actual, tip)
			}
			requireBranchCode(t, EstablishRemoteBranch(ctx, opts), workspacerevision.CodeConflict)
			after := runTestGit(t, opts.SourceURL, "show-ref", "--heads")
			if fork && before != after {
				t.Fatal("source refs changed")
			}
			if !fork {
				for _, line := range strings.Split(before, "\n") {
					if line != "" && !strings.Contains(after, line) {
						t.Fatal("original source branch changed")
					}
				}
			}
			if entries, err := os.ReadDir(opts.TempDir); err != nil || len(entries) != 0 {
				t.Fatalf("sterile transfer state retained: %v, %v", entries, err)
			}
		})
	}
}

func TestIntegrationRemoteBranchRefusals(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *RemoteBranchOptions, string)
		code   string
	}{
		{"source-grant", func(_ *testing.T, o *RemoteBranchOptions, _ string) { o.SourceRead.Authorized = false }, workspacerevision.CodeUnauthorized},
		{"target-grant", func(_ *testing.T, o *RemoteBranchOptions, _ string) { o.TargetWrite.Authorized = false }, workspacerevision.CodeUnauthorized},
		{"missing-sha", func(_ *testing.T, o *RemoteBranchOptions, _ string) { o.Binding.StartingSHA = strings.Repeat("f", 40) }, workspacerevision.CodeAcquisition},
		{"source-object-substitution", func(t *testing.T, o *RemoteBranchOptions, source string) {
			// The selected commit exists, but not in the authorized remote.
			// Its real source branch still resolves to a different valid commit.
			mustWriteFile(t, filepath.Join(source, "not-published.txt"), "selected only locally")
			runTestGit(t, source, "add", ".")
			runTestGit(t, source, "commit", "-m", "not available from authorized source")
			o.Binding.StartingSHA = strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
		}, workspacerevision.CodeAcquisition},
		{"conflicting-ref", func(t *testing.T, o *RemoteBranchOptions, _ string) {
			runTestGit(t, o.TargetURL, "update-ref", o.Binding.Ref, "HEAD")
		}, workspacerevision.CodeConflict},
		{"tag-object", func(t *testing.T, o *RemoteBranchOptions, source string) {
			runTestGit(t, source, "tag", "-a", "not-a-commit", "-m", "annotated tag")
			sha := strings.TrimSpace(runTestGit(t, source, "rev-parse", "refs/tags/not-a-commit"))
			runTestGit(t, source, "push", o.SourceURL, "refs/tags/not-a-commit")
			o.Binding.StartingSHA = sha
		}, workspacerevision.CodeObjectType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, source := remoteBranchFixture(t, true)
			before := runTestGit(t, opts.SourceURL, "show-ref", "--heads")
			tc.change(t, &opts, source)
			requireBranchCode(t, EstablishRemoteBranch(ctx, opts), tc.code)
			if after := runTestGit(t, opts.SourceURL, "show-ref", "--heads"); before != after {
				t.Fatal("refusal mutated source")
			}
		})
	}
}
