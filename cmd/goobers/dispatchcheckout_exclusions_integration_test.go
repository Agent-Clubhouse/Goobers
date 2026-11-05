//go:build integration

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationCheckoutWorkspaceExcludesHarnessArtifacts(t *testing.T) {
	testdep.Require(t, "git")
	bare := newBareRepoWithCommit(t, "main")
	previous := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return bare, nil }
	t.Cleanup(func() { checkoutCloneURL = previous })
	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widget")
	t.Setenv(executor.BranchNamespaceEnvVar, "e2e/")
	t.Setenv(executor.BaseBranchEnvVar, "main")
	t.Setenv(dispatcher.EnvWorkflow, "publication")
	t.Setenv(dispatcher.EnvRunID, "run-1")
	t.Setenv(dispatcher.EnvWorkspaceBranch, "")
	t.Setenv(dispatcher.EnvWorkspaceDelta, "")
	checkoutExclusionGit(t, bare, "branch", "e2e/existing", "main")

	for _, tc := range []struct {
		name     string
		mode     apiv1.WorkspaceMode
		existing bool
		child    bool
	}{
		{name: "fresh run branch", mode: apiv1.WorkspaceRepo},
		{name: "existing run branch", mode: apiv1.WorkspaceRepo, existing: true},
		{name: "read-only", mode: apiv1.WorkspaceRepoReadOnly},
		{name: "isolated child", mode: apiv1.WorkspaceRepo, child: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(dispatcher.EnvStageWorkspace, string(tc.mode))
			if tc.existing {
				t.Setenv(dispatcher.EnvWorkspaceBranch, "e2e/existing")
			}
			workspace := t.TempDir()
			ctx := context.Background()
			if tc.child {
				checkoutExclusionGit(t, workspace, "clone", "--quiet", "--branch", "main", bare, ".")
				ctx = context.WithValue(ctx, isolatedChildKey{}, true)
			}
			if err := checkoutRepoWorkspace(ctx, workspace, io.Discard, nil, ""); err != nil {
				t.Fatal(err)
			}
			assertCheckoutArtifactExclusions(t, workspace)
		})
	}
}

func assertCheckoutArtifactExclusions(t *testing.T, workspace string) {
	t.Helper()
	paths := []string{
		".goobers/prompt.md", ".goobers/mcp/home/logs/debug.log", ".goobers/result.json",
		".goober-assets/skill.md", "mutations.jsonl", "source.go",
		"nested/.goobers/config.json", "nested/.goober-assets/skill.md", "nested/mutations.jsonl",
	}
	for _, path := range paths {
		name := filepath.Join(workspace, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("synthetic test fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checkoutExclusionGit(t, workspace, "add", "-A")
	got := strings.TrimSpace(checkoutExclusionGit(t, workspace, "diff", "--cached", "--name-only"))
	want := "nested/.goober-assets/skill.md\nnested/.goobers/config.json\nnested/mutations.jsonl\nsource.go"
	if got != want {
		t.Fatalf("staged paths = %q, want project source %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("target .gitignore changed: %v", err)
	}
}

func TestIntegrationCheckoutIsolatedChildFailsWhenExclusionCannotBeInstalled(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.WithValue(context.Background(), isolatedChildKey{}, true)
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	workspace := t.TempDir()
	checkoutExclusionGit(t, workspace, "init", "--quiet")
	exclude := filepath.Join(workspace, ".git", "info", "exclude")
	if err := os.Remove(exclude); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(exclude, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkoutRepoWorkspace(ctx, workspace, io.Discard, nil, ""); err == nil || !strings.Contains(err.Error(), "exclude harness paths") {
		t.Fatalf("checkout error = %v, want mandatory exclusion failure", err)
	}
}

func checkoutExclusionGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
