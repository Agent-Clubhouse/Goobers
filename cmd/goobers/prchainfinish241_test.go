package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// seedClaimedIssueJournal writes a run journal whose claim stage (query-backlog)
// finished with the claimed item's scalar fields merged into its Outputs — what
// executor.mergeResultFileOutputs produces from claimed-item.json — so a later
// stage can recover the claimed issue the way the live path does.
func seedClaimedIssueJournal(t *testing.T, root, runID, id, title string) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "query-backlog",
		Attempt: 1,
		Status:  "success",
		Outputs: map[string]any{"id": id, "title": title},
	}); err != nil {
		t.Fatalf("seed stage.finished: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close seeded journal: %v", err)
	}
}

// TestOpenPRDerivesTitleAndFixesFromClaimedIssue is #241 Part 1: a loop-opened
// PR carries the claimed issue's title and a `Fixes #N` back-reference,
// recovered from the run journal; a repass for the same run stays idempotent.
func TestOpenPRDerivesTitleAndFixesFromClaimedIssue(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const runID = "run-1"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	seedClaimedIssueJournal(t, root, runID, "42", "Fix the flaky login test")

	t.Chdir(t.TempDir())
	if code, _, stderr := runArgs(t, "open-pr", root); code != 0 {
		t.Fatalf("open-pr: code = %d, stderr = %q", code, stderr)
	}

	var pr *fakePR
	server.mu.Lock()
	for _, p := range server.prs {
		pr = p
	}
	prCount := len(server.prs)
	server.mu.Unlock()
	if pr == nil {
		t.Fatal("no PR opened")
	}
	if pr.title != "Fix the flaky login test" {
		t.Fatalf("PR title = %q, want the claimed issue title", pr.title)
	}
	if !strings.Contains(pr.body, "Fixes #42") {
		t.Fatalf("PR body = %q, want a `Fixes #42` back-reference", pr.body)
	}
	if prCount != 1 {
		t.Fatalf("expected exactly 1 PR, got %d", prCount)
	}

	// Repass: same run, same stable branch — updates the same PR, not a duplicate.
	t.Chdir(t.TempDir())
	if code, _, stderr := runArgs(t, "open-pr", root); code != 0 {
		t.Fatalf("open-pr repass: code = %d, stderr = %q", code, stderr)
	}
	server.mu.Lock()
	prCount = len(server.prs)
	server.mu.Unlock()
	if prCount != 1 {
		t.Fatalf("expected still 1 PR after repass, got %d (a duplicate was opened)", prCount)
	}
}

// TestOpenPRFallsBackToGenericWithoutClaimedIssue: a run that claimed nothing
// (no journal claim stage) keeps the generic title and adds no `Fixes` line —
// so non-claiming workflows are unaffected.
func TestOpenPRFallsBackToGenericWithoutClaimedIssue(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	// No journal seeded.
	t.Chdir(t.TempDir())
	if code, _, stderr := runArgs(t, "open-pr", root); code != 0 {
		t.Fatalf("open-pr: code = %d, stderr = %q", code, stderr)
	}
	var pr *fakePR
	server.mu.Lock()
	for _, p := range server.prs {
		pr = p
	}
	server.mu.Unlock()
	if pr == nil {
		t.Fatal("no PR opened")
	}
	if pr.title != "Automated implementation" {
		t.Fatalf("PR title = %q, want the generic fallback", pr.title)
	}
	if strings.Contains(pr.body, "Fixes #") {
		t.Fatalf("PR body = %q, should carry no Fixes reference without a claimed issue", pr.body)
	}
}

// The close-out resume-idempotency (#241 Part 2) is asserted in
// issuecloseout_test.go's TestIssueCloseOutNoLiveClaimIsResumeNoOp.
