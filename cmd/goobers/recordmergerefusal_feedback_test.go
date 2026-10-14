package main

import (
	"slices"
	"testing"

	"github.com/goobers/goobers/providers"
)

const unansweredFeedbackRefusal = unansweredHumanFeedbackReasonCode +
	": alice left review feedback at 2026-10-01T12:00:00Z that no Goobers response has acknowledged"

// #6134: a refusal for unanswered human feedback routes the PR to remediation
// (needs-remediation added, merge-ready removed) and never counts toward
// demotion — at threshold-many repeats the PR is still not demoted and no
// demotion sticky is written.
func TestRecordMergeRefusalRoutesUnansweredFeedbackToRemediation(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(79, "goobers/implementation/asked", "main", "sha-asked", "base1", false, nil, nil)
	server.addIssue(79, "pr with feedback")
	server.mu.Lock()
	server.issues[79].labels = []string{mergeReadyLabel}
	server.mu.Unlock()
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "79")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-asked")
	t.Setenv("GOOBERS_INPUT_REASON", unansweredFeedbackRefusal)
	t.Chdir(t.TempDir())

	for attempt := 1; attempt <= defaultDemotionThreshold; attempt++ {
		if code, _, stderr := runArgs(t, "record-merge-refusal", root); code != 0 {
			t.Fatalf("attempt %d: code = %d, stderr = %q", attempt, code, stderr)
		}
	}
	server.mu.Lock()
	labels := append([]string(nil), server.issues[79].labels...)
	comments := len(server.issues[79].comments)
	server.mu.Unlock()
	if !slices.Contains(labels, needsRemediationLabel) || slices.Contains(labels, mergeReadyLabel) || slices.Contains(labels, mergeDemotedLabel) {
		t.Fatalf("labels = %v, want %s added, %s removed and no demotion", labels, needsRemediationLabel, mergeReadyLabel)
	}
	if comments != 0 {
		t.Fatalf("comments = %d, want no demotion sticky for a feedback refusal", comments)
	}
}

// On Azure DevOps the routing labels are pull-request labels, never the work
// item that shares the PR's number.
func TestRecordMergeRefusalRoutesUnansweredFeedbackADO(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	st := &adoRemediationServerState{
		owner: repo.Owner, project: repo.Project, name: repo.Name,
		prNumber: 80, prBranch: "goobers/impl/asked", base: "main",
		headSHA: "sha-asked", baseSHA: "base1", status: "active",
		labels: []string{mergeReadyLabel},
	}
	server := st.start(t)
	installADOStageProvider(t, repo, server)
	t.Setenv("GOOBERS_RUN_ID", "run-6134-ado")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	setDeliveredADOStageCredentials(t)
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "80")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-asked")
	t.Setenv("GOOBERS_INPUT_REASON", unansweredFeedbackRefusal)
	t.Chdir(t.TempDir())

	if code, stdout, stderr := runArgs(t, "record-merge-refusal", root); code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !slices.Contains(st.labels, needsRemediationLabel) || slices.Contains(st.labels, mergeReadyLabel) {
		t.Fatalf("labels = %v, want %s added and %s removed", st.labels, needsRemediationLabel, mergeReadyLabel)
	}
	if st.workItemHit {
		t.Fatal("a wit/workitems endpoint was called — the PR-as-work-item wrong-object hazard")
	}
}
