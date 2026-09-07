package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

// TestPRSelectReportsSevenEscalatedPullRequestsRatherThanBareNoWork is #2969's
// named regression case.
//
// Live on a production instance 2026-08-15: seven open implementation PRs (#533-#539) were
// non-draft, mergeable CLEAN and green, and every one carried
// goobers:merge-escalated. Run de97c14bcaadb32fafb864d100eae0d7 completed
// successfully with pr-select no-work, and `goobers status` reported a 100%
// merge-review success rate with no indication that seven pull requests were
// excluded. The scheduler was healthy and the queue was operationally dead,
// and nothing distinguished the two.
func TestPRSelectReportsSevenEscalatedPullRequestsRatherThanBareNoWork(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for number := 533; number <= 539; number++ {
		server.addOpenPR(number, "goobers/implementation/run-"+strconv.Itoa(number), "main",
			"head", "base", false, []string{remediationEscalatedLabel}, nil)
		server.addIssue(number, "escalated pr")
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), filepath.Join(workDir, "selected-pr.json"))

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "queue parked") {
		t.Fatalf("stdout = %q, want the no-work cycle to say the queue is parked, not merely that "+
			"there was nothing to do", stdout)
	}
	if !strings.Contains(stdout, "7 of 7") {
		t.Fatalf("stdout = %q, want the count of excluded matching pull requests", stdout)
	}
	if !strings.Contains(stdout, exclusionEscalated) {
		t.Fatalf("stdout = %q, want the normalized reason %q so an operator can act without reading "+
			"pr-select's source", stdout, exclusionEscalated)
	}
}

// TestPRSelectDistinguishesAnEmptyQueueFromAParkedOne is the acceptance
// criterion that carries the operational value: "no pull requests" and "every
// pull request parked" are the same bare no-work today, and they call for
// opposite responses.
func TestPRSelectDistinguishesAnEmptyQueueFromAParkedOne(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), filepath.Join(workDir, "selected-pr.json"))

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "queue empty") {
		t.Fatalf("stdout = %q, want an empty queue reported as empty, not parked", stdout)
	}
	if strings.Contains(stdout, "queue parked") {
		t.Fatalf("stdout = %q, want no parked-queue claim when there are no pull requests at all", stdout)
	}
}

// TestPRSelectExclusionSummaryVocabulary pins the rendering at the unit,
// including the tallying order and the three states the summary must keep
// apart. Reason order is first-seen so the line is deterministic across runs.
func TestPRSelectExclusionSummaryVocabulary(t *testing.T) {
	t.Run("empty queue", func(t *testing.T) {
		e := newPRSelectExclusions()
		if got := e.summary(); !strings.Contains(got, "queue empty") {
			t.Fatalf("summary = %q, want an empty-queue line", got)
		}
	})

	t.Run("parked queue counts by reason", func(t *testing.T) {
		e := newPRSelectExclusions()
		e.matching = 4
		e.record(exclusionEscalated)
		e.record(exclusionEscalated)
		e.record(exclusionDraft)
		e.record(exclusionSiblingBlocked)

		got := e.summary()
		for _, want := range []string{
			"queue parked", "4 of 4",
			exclusionEscalated + " 2", exclusionDraft + " 1", exclusionSiblingBlocked + " 1",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("summary = %q, want it to contain %q", got, want)
			}
		}
		// First-seen order, so the line does not churn between runs over the
		// same queue state.
		if idx := strings.Index(got, exclusionDraft); idx < strings.Index(got, exclusionEscalated) {
			t.Fatalf("summary = %q, want reasons in first-seen order", got)
		}
	})

	t.Run("matching but not excluded here", func(t *testing.T) {
		e := newPRSelectExclusions()
		e.matching = 2
		got := e.summary()
		if strings.Contains(got, "queue parked") || strings.Contains(got, "queue empty") {
			t.Fatalf("summary = %q, want the third state: pull requests exist and none was excluded "+
				"by these gates, so the loss happened later in selection", got)
		}
	})
}
