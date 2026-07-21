package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// recordClaimedIssue writes the query-backlog stage.finished event open-pr reads
// back (claimedIssueFromJournal) to recover the run's claimed issue id/title.
func recordClaimedIssue(t *testing.T, root, runID, id, title string) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "query-backlog", Attempt: 1, Status: "success",
		Outputs: map[string]any{"id": id, "title": title},
	}); err != nil {
		t.Fatalf("record claimed issue: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func readOpenPRResult(t *testing.T, workDir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, "pr-result.json"))
	if err != nil {
		t.Fatalf("read pr-result.json: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal pr-result.json: %v", err)
	}
	return result
}

// TestOpenPRAbortsWhenClaimedIssueClosedMidFlight is #947: an issue closed after
// it was claimed (at query-backlog) but before open-pr must NOT still produce a
// PR — that would burn a merge-review cycle and one of the scarce open-PR slots
// on work that is already moot. open-pr re-checks the issue state immediately
// before opening; finding it closed, it opens nothing and emits opened=false,
// which the open-pr-gate routes to @abort.
func TestOpenPRAbortsWhenClaimedIssueClosedMidFlight(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const runID = "run-stale"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)

	server.addIssue(684, "Superseded work")
	server.closeIssue(684)
	recordClaimedIssue(t, root, runID, "684", "Superseded work")

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no longer open") {
		t.Errorf("stdout = %q, want a clear no-longer-open reason", stdout)
	}
	server.mu.Lock()
	prCount := len(server.prs)
	server.mu.Unlock()
	if prCount != 0 {
		t.Fatalf("expected no PR opened for a closed issue, got %d", prCount)
	}
	result := readOpenPRResult(t, workDir)
	if result["opened"] != "false" {
		t.Errorf("opened = %q, want \"false\"", result["opened"])
	}
	if _, ok := result["prNumber"]; ok {
		t.Errorf("prNumber must be absent on the aborted path, got %q", result["prNumber"])
	}
}

// TestOpenPRProceedsWhenClaimedIssueStillOpen is #947's positive path: a still-
// open claimed issue opens a PR exactly as before, with opened=true so the
// open-pr-gate proceeds to ci-poll.
func TestOpenPRProceedsWhenClaimedIssueStillOpen(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const runID = "run-fresh"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)

	server.addIssue(685, "Live work")
	recordClaimedIssue(t, root, runID, "685", "Live work")

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	server.mu.Lock()
	prCount := len(server.prs)
	server.mu.Unlock()
	if prCount != 1 {
		t.Fatalf("expected one PR opened for an open issue, got %d", prCount)
	}
	result := readOpenPRResult(t, workDir)
	if result["opened"] != "true" {
		t.Errorf("opened = %q, want \"true\"", result["opened"])
	}
	if result["prNumber"] == "" {
		t.Errorf("prNumber missing on the opened path: %+v", result)
	}
}
