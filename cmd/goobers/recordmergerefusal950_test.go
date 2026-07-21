package main

import (
	"strings"
	"testing"
)

func issueHasLabel(server *fakeGitHubServer, number int, label string) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, l := range server.issues[number].labels {
		if l == label {
			return true
		}
	}
	return false
}

// TestRecordMergeRefusalDemotesAfterThreshold is #950's recorder end to end:
// consecutive merge refusals at an unchanged head accrue toward the threshold,
// and the crossing one applies goobers:merge-demoted so the election can crown a
// sibling instead.
func TestRecordMergeRefusalDemotesAfterThreshold(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(77, "goobers/implementation/stuck", "main", "sha-stuck", "base1", false, nil, nil)
	server.addIssue(77, "stuck lander")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-stuck")
	t.Setenv("GOOBERS_INPUT_REASON", "base moved: verdict pinned to base1, PR is now based on base2, and that movement touches files this PR also changes")

	workDir := t.TempDir()
	t.Chdir(workDir)

	for attempt := 1; attempt <= defaultDemotionThreshold; attempt++ {
		code, stdout, stderr := runArgs(t, "record-merge-refusal", root)
		if code != 0 {
			t.Fatalf("attempt %d: code = %d, stderr = %q", attempt, code, stderr)
		}
		demoted := issueHasLabel(server, 77, mergeDemotedLabel)
		switch {
		case attempt < defaultDemotionThreshold && demoted:
			t.Fatalf("attempt %d: demoted too early (stdout=%q)", attempt, stdout)
		case attempt == defaultDemotionThreshold && !demoted:
			t.Fatalf("attempt %d: expected goobers:merge-demoted after crossing the threshold (stdout=%q)", attempt, stdout)
		}
	}
}

// TestRecordMergeRefusalSkipsAdvisory proves an advisory-mode "refusal" (no real
// merge attempted) never accrues toward demotion — otherwise advisory mode would
// demote every lander every cycle.
func TestRecordMergeRefusalSkipsAdvisory(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(80, "goobers/implementation/adv", "main", "sha-adv", "base1", false, nil, nil)
	server.addIssue(80, "advisory pr")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "80")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-adv")
	t.Setenv("GOOBERS_INPUT_REASON", "advisory mode: no merge attempted")
	t.Setenv("GOOBERS_INPUT_DEMOTIONTHRESHOLD", "1") // would demote on the first real refusal

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "record-merge-refusal", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "advisory") {
		t.Errorf("stdout = %q, want an advisory-mode skip message", stdout)
	}
	if issueHasLabel(server, 80, mergeDemotedLabel) {
		t.Fatal("an advisory-mode result must not demote the PR")
	}
}

// TestRecordMergeRefusalResetsOnHeadAdvance proves a refusal at a NEW head resets
// the counter — a PR whose head advanced (a remediation push) is a genuinely
// fresh attempt, not a continuation of the stuck run.
func TestRecordMergeRefusalResetsOnHeadAdvance(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	// The PR's live head is new-head, but the recorded prior refusals were at
	// old-head — so the count must reset rather than accumulate to demotion.
	server.addOpenPR(81, "goobers/implementation/moved", "main", "new-head", "base1", false, nil, nil)
	server.addIssue(81, "moved pr")
	prior, err := mergeDemotionComment(mergeDemotionState{Attempts: 2, Demoted: false, HeadSHA: "old-head"})
	if err != nil {
		t.Fatalf("mergeDemotionComment: %v", err)
	}
	server.addComment(81, prior)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "81")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "new-head")
	t.Setenv("GOOBERS_INPUT_REASON", "base moved: touches this PR's files")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "record-merge-refusal", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if issueHasLabel(server, 81, mergeDemotedLabel) {
		t.Fatal("a refusal at a new head must reset the counter, not demote (prior attempts were at a different head)")
	}
	if !strings.Contains(stdout, "1/") {
		t.Errorf("stdout = %q, want the counter reset to attempt 1", stdout)
	}
}
