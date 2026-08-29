package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

func TestPRSelectSupportsMultipleHeadPrefixes(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(11, "goobers/docs-updater/run-11", "main", "docs-head", "base",
		false, nil, []fakePRFile{{path: "docs/guide.md", status: "modified"}})
	server.setPRIdentities(11, "octocat", []string{"maintainer"}, []string{"reviewer"})
	server.addOpenPR(9, "goobers/docs-updater/run-9", "main", "other-docs-head", "base",
		false, nil, []fakePRFile{{path: "docs/other.md", status: "modified"}})
	server.setPRIdentities(9, "someone-else", []string{"maintainer"}, []string{"reviewer"})
	server.addOpenPR(10, "goobers/tutor/run-10", "main", "tutor-head", "base",
		false, nil, []fakePRFile{{path: "reference-workflows/gaggle.yaml", status: "modified"}})

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv(executor.RepoProviderEnvVar, "github")
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv(executor.InputEnvVar("headPrefixes"), "goobers/implementation/, goobers/docs-updater/")
	t.Setenv(executor.InputEnvVar("author"), "octocat")
	t.Setenv(executor.InputEnvVar("assignee"), "maintainer")
	t.Setenv(executor.InputEnvVar("requestedReviewer"), "reviewer")
	workDir := t.TempDir()
	t.Chdir(workDir)

	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selected-pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	var selected map[string]string
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatal(err)
	}
	if selected["number"] != "11" {
		t.Fatalf("selected PR = %q, want docs-updater PR 11; lower-numbered tutor must remain excluded", selected["number"])
	}
}
