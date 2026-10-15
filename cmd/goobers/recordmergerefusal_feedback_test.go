package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func unansweredFeedbackRefusal(commentID string) string {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return unansweredHumanFeedbackReason(providers.Comment{ID: commentID, Author: "alice", CreatedAt: &at})
}

func TestFeedbackCommentTokenRoundTrips(t *testing.T) {
	key, at, ok := parseFeedbackCommentToken(unansweredFeedbackRefusal("501"))
	if !ok || key != "501" || !at.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("token = %q %v %v, want 501 at 12:00", key, at, ok)
	}
	if _, _, ok := parseFeedbackCommentToken(unansweredHumanFeedbackReasonCode + ": legacy reason"); ok {
		t.Fatal("a reason without a token parsed as keyed")
	}
}

// #6134: the first refusal over a comment routes the PR to remediation
// (needs-remediation added, merge-ready removed, a sticky record written) and
// never counts toward demotion. A second refusal over the SAME comment — a
// remediation pass did not answer it — parks the PR needs-human with a comment
// that acknowledges the feedback, instead of cycling review and remediation
// forever. A refusal over a different, newer comment routes again.
func TestRecordMergeRefusalRoutesThenParksUnansweredFeedback(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(79, "goobers/implementation/asked", "main", "sha-asked", "base1", false, nil, nil)
	server.addIssue(79, "pr with feedback")
	setLabels := func(labels ...string) {
		server.mu.Lock()
		server.issues[79].labels = labels
		server.mu.Unlock()
	}
	snapshot := func() ([]string, []string) {
		server.mu.Lock()
		defer server.mu.Unlock()
		return append([]string(nil), server.issues[79].labels...), append([]string(nil), server.issues[79].comments...)
	}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "79")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-asked")
	t.Chdir(t.TempDir())
	refuse := func(reason string) {
		t.Helper()
		t.Setenv("GOOBERS_INPUT_REASON", reason)
		if code, _, stderr := runArgs(t, "record-merge-refusal", root); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr)
		}
	}

	setLabels(mergeReadyLabel)
	refuse(unansweredFeedbackRefusal("501"))
	labels, comments := snapshot()
	if !slices.Contains(labels, needsRemediationLabel) || slices.Contains(labels, mergeReadyLabel) || slices.Contains(labels, mergeDemotedLabel) {
		t.Fatalf("first refusal labels = %v, want routed to remediation without demotion", labels)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], feedbackRefusalPayloadPrefix) {
		t.Fatalf("first refusal comments = %q, want one sticky refusal record", comments)
	}

	// Remediation ran (and cleared its label), review passed again, and the
	// same comment is still unanswered.
	setLabels(mergeReadyLabel)
	refuse(unansweredFeedbackRefusal("501"))
	labels, comments = snapshot()
	if !slices.Contains(labels, providers.LabelNeedsHuman) || slices.Contains(labels, needsRemediationLabel) || slices.Contains(labels, mergeReadyLabel) {
		t.Fatalf("repeat refusal labels = %v, want parked %s", labels, providers.LabelNeedsHuman)
	}
	if len(comments) != 2 || !strings.Contains(comments[1], feedbackAckMarkerPrefix+"2026-10-01T12:00:00Z") {
		t.Fatalf("repeat refusal comments = %q, want a park explanation acknowledging the comment", comments)
	}
	if goobersBookkeepingComment(comments[1]) {
		t.Fatal("the park explanation is bookkeeping; it must acknowledge the feedback it answers")
	}

	// A different, newer comment is new feedback: route again.
	setLabels(mergeReadyLabel)
	refuse(unansweredFeedbackRefusal("777"))
	labels, _ = snapshot()
	if !slices.Contains(labels, needsRemediationLabel) {
		t.Fatalf("new-comment refusal labels = %v, want routed to remediation again", labels)
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
	t.Setenv("GOOBERS_INPUT_REASON", unansweredHumanFeedbackReasonCode+": alice left review feedback that no Goobers response has acknowledged")
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
