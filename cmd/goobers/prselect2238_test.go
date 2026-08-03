package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

// TestPRSelectAlwaysExcludesRunAbortedLabel is #2238's pr-select acceptance
// criterion: a PR labeled goobers:run-aborted (its originating implementation
// run was cancelled) must never be eligible for merge-review selection, even
// if a caller's excludeLabels input omits it — same always-on treatment as
// noMergeReviewLabel.
func TestPRSelectAlwaysExcludesRunAbortedLabel(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(2238, "goobers/implementation/run-2238", "main", "aborted-head", "main-base", false, []string{abortedRunLabel}, nil)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want no work", code, stdout, stderr)
	}
	assertNoWorkProviderStageResult(t, resultFile)
}
