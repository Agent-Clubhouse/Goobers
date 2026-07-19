package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPRSelectDefaultExcludesTutorBranches(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(894, "goobers/tutor/run-894", "main", "tutor-head", "main-base", false, nil, nil)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want no work", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(workDir, "selected-pr.json")); !os.IsNotExist(err) {
		t.Fatalf("selected-pr.json stat error = %v, want not exist", err)
	}
}
