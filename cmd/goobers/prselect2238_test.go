package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
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

func TestPRSelectClearsStaleRunAbortedLabelOnGreenUnreviewedPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(5437, "green parked PR", abortedRunLabel)
	server.addOpenPR(5437, "goobers/implementation/run-5437", "main", "green-head", "main-base", false, []string{abortedRunLabel}, nil)
	server.setPRMergeable(5437, true)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #5437") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want PR selected after stale run-aborted clear", code, stdout, stderr)
	}
	assertFakeIssueLabels(t, server, 5437, nil, []string{abortedRunLabel})
	server.mu.Lock()
	comments := append([]string(nil), server.issues[5437].comments...)
	server.mu.Unlock()
	if len(comments) != 0 {
		t.Fatalf("comments = %q, want stale-label repair to preserve the zero-comment merge-review predicate", comments)
	}
}

func TestPRSelectLeavesRunAbortedPRWithCommentsParked(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(5438, "commented parked PR", abortedRunLabel)
	server.addOpenPR(5438, "goobers/implementation/run-5438", "main", "green-head", "main-base", false, []string{abortedRunLabel}, nil)
	server.setPRMergeable(5438, true)
	server.addRawCommentAs(5438, "reviewer", "needs a human decision")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want no work for commented run-aborted PR", code, stdout, stderr)
	}
	assertNoWorkProviderStageResult(t, resultFile)
	assertFakeIssueLabels(t, server, 5438, []string{abortedRunLabel}, nil)
}

func TestPRSelectIgnoresGoobersCommentsFromOtherInstanceIdentities(t *testing.T) {
	const number = 6766
	server := newReviewedRunAbortedPRFixture(t, number)
	for i, author := range []string{"jeffstei", "goobersbot"} {
		body, err := providers.StampAttribution(
			"automation status",
			providers.Attribution{
				InstanceID: strings.Repeat(string(rune('a'+i)), 32),
				Gaggle:     "goobers",
				Workflow:   "merge-review",
				Task:       "apply-verdict",
				Goober:     "deterministic",
				Run:        "merge-review-run-" + author,
			},
			"comment",
		)
		if err != nil {
			t.Fatal(err)
		}
		server.addRawCommentAs(number, author, body)
	}

	root := initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #6766") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want PR selected despite other-instance Goobers comments", code, stdout, stderr)
	}
	assertFakeIssueLabels(t, server, number, nil, []string{abortedRunLabel})
}

func TestPRSelectClearsRunAbortedForCurrentHeadPassFromOtherIdentity(t *testing.T) {
	const number = 6767
	server := newReviewedRunAbortedPRFixture(t, number)
	pass := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Summary:  "verified",
		HeadSHA:  "green-head",
		BaseSHA:  "main-base",
	})
	body, err := providers.StampAttribution(
		pass,
		providers.Attribution{
			InstanceID: strings.Repeat("b", 32),
			Gaggle:     "goobers",
			Workflow:   "merge-review",
			Task:       "apply-verdict",
			Goober:     "deterministic",
			Run:        "merge-review-pass",
		},
		"comment",
	)
	if err != nil {
		t.Fatal(err)
	}
	server.addRawCommentAs(number, "jeffstei", body)

	root := initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #6767") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want current-head pass to clear run-aborted", code, stdout, stderr)
	}
	assertFakeIssueLabels(t, server, number, nil, []string{abortedRunLabel})
}

func TestPRSelectClearsRunAbortedAfterVerifiedSuccessfulRemediation(t *testing.T) {
	const number = 6407
	abortedAt := time.Date(2026, 10, 1, 19, 47, 43, 0, time.UTC)
	remediatedAt := abortedAt.Add(2 * time.Hour)
	server := newReviewedRunAbortedPRFixture(t, number)
	server.setLabelEventTime(number, abortedRunLabel, true, abortedAt)
	server.addRawCommentAtAsType(number, "reviewer", "", "substantive finding", remediatedAt.Add(-time.Hour))
	response, err := providers.StampAttribution(
		remediationResponseMarker("remediation-run"),
		providers.Attribution{
			InstanceID: strings.Repeat("c", 32),
			Gaggle:     "goobers",
			Workflow:   "pr-remediation",
			Task:       "respond",
			Goober:     "deterministic",
			Run:        "remediation-run",
		},
		"comment",
	)
	if err != nil {
		t.Fatal(err)
	}
	server.addRawCommentAtAsType(number, "jeffstei", "", response, remediatedAt)

	root := initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #6407") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want remediated PR selected", code, stdout, stderr)
	}
	assertFakeIssueLabels(t, server, number, nil, []string{abortedRunLabel})
}

func TestPRSelectLeavesVerifiedRemediationParkedAfterNewerAbort(t *testing.T) {
	const number = 6408
	remediatedAt := time.Date(2026, 10, 1, 21, 21, 22, 0, time.UTC)
	server := newReviewedRunAbortedPRFixture(t, number)
	server.addCommentAtAs(number, "goobers", remediationResponseMarker("remediation-run"), remediatedAt)
	server.setLabelEventTime(number, abortedRunLabel, true, remediatedAt.Add(time.Minute))

	runPRSelectExpectingRunAbortedPark(t, server, number)
}

func TestPRSelectLeavesVerifiedRemediationWithUnresolvedThreadParked(t *testing.T) {
	const number = 6409
	abortedAt := time.Date(2026, 10, 1, 19, 47, 43, 0, time.UTC)
	server := newReviewedRunAbortedPRFixture(t, number)
	server.setLabelEventTime(number, abortedRunLabel, true, abortedAt)
	server.addCommentAtAs(number, "goobers", remediationResponseMarker("remediation-run"), abortedAt.Add(time.Hour))
	server.addPRInlineReviewComment(number)

	runPRSelectExpectingRunAbortedPark(t, server, number)
}

func TestPRSelectLeavesVerifiedRemediationUnderHumanHoldParked(t *testing.T) {
	const number = 6410
	abortedAt := time.Date(2026, 10, 1, 19, 47, 43, 0, time.UTC)
	server := newReviewedRunAbortedPRFixture(t, number)
	server.setLabelEventTime(number, abortedRunLabel, true, abortedAt)
	server.addCommentAtAs(number, "goobers", remediationResponseMarker("remediation-run"), abortedAt.Add(time.Hour))
	server.mu.Lock()
	server.prs[number].labels = append(server.prs[number].labels, providers.LabelNeedsHuman)
	server.issues[number].labels = append(server.issues[number].labels, providers.LabelNeedsHuman)
	server.mu.Unlock()

	runPRSelectExpectingRunAbortedPark(t, server, number)
}

func TestPRSelectRestoresRunAbortedAfterConcurrentNewerAbort(t *testing.T) {
	const number = 6411
	abortedAt := time.Date(2026, 10, 1, 19, 47, 43, 0, time.UTC)
	remediatedAt := abortedAt.Add(time.Hour)
	server := newReviewedRunAbortedPRFixture(t, number)
	server.setLabelEventTime(number, abortedRunLabel, true, abortedAt)
	server.addCommentAtAs(number, "goobers", remediationResponseMarker("remediation-run"), remediatedAt)
	server.mutatePullRequestAfterLabelRemoval(number, func(s *fakeGitHubServer, pr *fakePR) {
		pr.labels = append(pr.labels, abortedRunLabel)
		s.issues[number].labels = append(s.issues[number].labels, abortedRunLabel)
		s.appendLabelEventAsLocked(number, abortedRunLabel, true, remediatedAt.Add(time.Minute), "canceller")
	})

	runPRSelectExpectingRunAbortedPark(t, server, number)
}

func TestPRSelectRestoresRunAbortedAfterConcurrentUnresolvedThread(t *testing.T) {
	const number = 6412
	abortedAt := time.Date(2026, 10, 1, 19, 47, 43, 0, time.UTC)
	server := newReviewedRunAbortedPRFixture(t, number)
	server.setLabelEventTime(number, abortedRunLabel, true, abortedAt)
	server.addCommentAtAs(number, "goobers", remediationResponseMarker("remediation-run"), abortedAt.Add(time.Hour))
	server.mutatePullRequestAfterLabelRemoval(number, func(_ *fakeGitHubServer, pr *fakePR) {
		pr.inlineComments = append(pr.inlineComments, fakeInlineReviewComment{
			id: 1, body: "new unresolved finding", path: "main.go", line: 1, thread: "thread-1",
		})
	})

	runPRSelectExpectingRunAbortedPark(t, server, number)
}

func TestPRSelectRestoresRunAbortedAfterConcurrentHumanHold(t *testing.T) {
	const number = 6413
	abortedAt := time.Date(2026, 10, 1, 19, 47, 43, 0, time.UTC)
	server := newReviewedRunAbortedPRFixture(t, number)
	server.setLabelEventTime(number, abortedRunLabel, true, abortedAt)
	server.addCommentAtAs(number, "goobers", remediationResponseMarker("remediation-run"), abortedAt.Add(time.Hour))
	server.mutatePullRequestAfterLabelRemoval(number, func(s *fakeGitHubServer, pr *fakePR) {
		pr.labels = append(pr.labels, providers.LabelNeedsHuman)
		s.issues[number].labels = append(s.issues[number].labels, providers.LabelNeedsHuman)
	})

	runPRSelectExpectingRunAbortedPark(t, server, number)
}

func TestPRSelectLeavesRunAbortedPRWithChangesRequestedReviewParked(t *testing.T) {
	server := newReviewedRunAbortedPRFixture(t, 5439)
	server.addPRReview(5439, "CHANGES_REQUESTED")

	runPRSelectExpectingRunAbortedPark(t, server, 5439)
}

func TestPRSelectLeavesRunAbortedPRWithInlineReviewCommentParked(t *testing.T) {
	server := newReviewedRunAbortedPRFixture(t, 5440)
	server.addPRInlineReviewComment(5440)

	runPRSelectExpectingRunAbortedPark(t, server, 5440)
}

func TestPRSelectLeavesRunAbortedPRWithApprovedReviewParked(t *testing.T) {
	server := newReviewedRunAbortedPRFixture(t, 5441)
	server.addPRReview(5441, "APPROVED")

	runPRSelectExpectingRunAbortedPark(t, server, 5441)
}

func TestPRSelectLeavesRunAbortedPRParkedWhenReviewAttentionReadFails(t *testing.T) {
	server := newReviewedRunAbortedPRFixture(t, 5442)
	server.setPullRequestReviewThreadsFailure(5442, 500)

	runPRSelectExpectingRunAbortedPark(t, server, 5442)
}

func TestPRSelectLeavesRunAbortedPRParkedWhenLivePollNoLongerGreen(t *testing.T) {
	server := newReviewedRunAbortedPRFixture(t, 5443)
	server.mutatePullRequestOnNextGet(5443, func(s *fakeGitHubServer, pr *fakePR) {
		pr.headSHA = "new-head-pending"
		pr.checkState = "pending"
		issue := s.issues[5443]
		s.nextCommentID++
		issue.comments = append(issue.comments, "review comment arrived after list")
		issue.commentIDs = append(issue.commentIDs, s.nextCommentID)
		issue.commentAuthors = append(issue.commentAuthors, "reviewer")
		issue.commentTypes = append(issue.commentTypes, "User")
		issue.commentTimes = append(issue.commentTimes, time.Time{})
	})

	runPRSelectExpectingRunAbortedPark(t, server, 5443)
}

func TestPRSelectUsesLivePollSnapshotAfterClearingRunAbortedLabel(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(5444, "green parked PR", abortedRunLabel)
	server.addOpenPR(5444, "goobers/implementation/run-5444", "main", "old-head", "main-base", false, []string{abortedRunLabel}, nil)
	server.setPRMergeable(5444, true)
	server.mutatePullRequestOnNextGet(5444, func(_ *fakeGitHubServer, pr *fakePR) {
		pr.headSHA = "fresh-green-head"
		pr.checkState = "success"
	})

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #5444") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want PR selected after live stale-label clear", code, stdout, stderr)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		HeadSHA string `json:"headSha"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.HeadSHA != "fresh-green-head" {
		t.Fatalf("selected result headSha = %q, want live poll head", result.HeadSHA)
	}
	assertFakeIssueLabels(t, server, 5444, nil, []string{abortedRunLabel})
}

func TestPRSelectUsesLivePollSnapshotWhenRunAbortedAlreadyCleared(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(5445, "green parked PR", abortedRunLabel)
	server.addOpenPR(5445, "goobers/implementation/run-5445", "main", "old-head", "main-base", false, []string{abortedRunLabel}, nil)
	server.setPRMergeable(5445, true)
	server.mutatePullRequestOnNextGet(5445, func(s *fakeGitHubServer, pr *fakePR) {
		pr.headSHA = "already-cleared-head"
		pr.labels = removeLabel(pr.labels, abortedRunLabel)
		s.issues[5445].labels = removeLabel(s.issues[5445].labels, abortedRunLabel)
	})

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "selected PR #5445") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want PR selected from live no-label snapshot", code, stdout, stderr)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		HeadSHA string `json:"headSha"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.HeadSHA != "already-cleared-head" {
		t.Fatalf("selected result headSha = %q, want live no-label head", result.HeadSHA)
	}
	assertFakeIssueLabels(t, server, 5445, nil, []string{abortedRunLabel})
}

func TestPRSelectDoesNotUseAlreadyClearedRunAbortedSnapshotWhenLiveChecksPending(t *testing.T) {
	server := newReviewedRunAbortedPRFixture(t, 5446)
	server.mutatePullRequestOnNextGet(5446, func(s *fakeGitHubServer, pr *fakePR) {
		pr.headSHA = "already-cleared-pending-head"
		pr.checkState = "pending"
		pr.labels = removeLabel(pr.labels, abortedRunLabel)
		s.issues[5446].labels = removeLabel(s.issues[5446].labels, abortedRunLabel)
	})

	root := initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want no work for live pending no-label snapshot", code, stdout, stderr)
	}
	assertNoWorkProviderStageResult(t, resultFile)
	assertFakeIssueLabels(t, server, 5446, nil, []string{abortedRunLabel})
}

func TestRunAbortedLivePollSnapshotPreservesLabelsWhenProviderOmitsThem(t *testing.T) {
	listed := providers.PullRequestSummary{
		Number:     5447,
		Labels:     []string{abortedRunLabel},
		CheckState: providers.CheckStatePassing,
	}
	poll := providers.PullRequestPollResult{
		Number:     5447,
		State:      "open",
		CheckState: providers.CheckStatePassing,
	}

	refreshed := pullRequestSummaryFromPoll(listed, poll)
	if !reflect.DeepEqual(refreshed.Labels, listed.Labels) {
		t.Fatalf("refreshed labels = %v, want listed labels preserved when poll omits labels", refreshed.Labels)
	}
}

func newReviewedRunAbortedPRFixture(t *testing.T, number int) *fakeGitHubServer {
	t.Helper()
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(number, "reviewed parked PR", abortedRunLabel)
	server.addOpenPR(number, "goobers/implementation/run-reviewed", "main", "green-head", "main-base", false, []string{abortedRunLabel}, nil)
	server.setPRMergeable(number, true)
	return server
}

func runPRSelectExpectingRunAbortedPark(t *testing.T, server *fakeGitHubServer, number int) {
	t.Helper()
	root := initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q; want no work for reviewed run-aborted PR", code, stdout, stderr)
	}
	assertNoWorkProviderStageResult(t, resultFile)
	assertFakeIssueLabels(t, server, number, []string{abortedRunLabel}, nil)
}
