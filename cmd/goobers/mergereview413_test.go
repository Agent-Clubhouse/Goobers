package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// threadOutput reproduces the runner's stage-to-stage handoff for a single
// scalar result-file key, so a test threads a value between stages the way
// production does instead of hardcoding the next stage's env var (which is
// exactly how the 100%-broken merge-review wiring passed the older stubbed-IO
// test — #413). Two real mechanisms compose:
//   - executor.mergeResultFileOutputs keeps a result file's string/number/bool
//     scalars as the stage's Outputs (a JSON number becomes a float64), and
//   - executor.buildStageEnv, threading those Outputs into the next stage's
//     GOOBERS_INPUT_* env, stringifies ONLY string-typed inputs (SEC-045).
//
// Net: a scalar output reaches the next stage IFF its JSON value is a string.
// A numeric selectedNumber is silently dropped — the #413 bug. threaded=false
// here is that drop, faithfully reproduced.
func threadOutput(t *testing.T, resultFile, key string) (value string, threaded bool) {
	t.Helper()
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read %s: %v", resultFile, err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", resultFile, err)
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, isString := v.(string) // buildStageEnv threads only string-typed inputs
	return s, isString
}

// TestMergeReviewThreadsSelectedNumberToApplyVerdict is #413's acceptance: the
// full pr-select -> gather-sibling-context -> (review verdict) -> apply-verdict
// chain, with selectedNumber threaded between stages via the REAL runner rule
// (threadOutput), asserts a label is actually applied for an eligible PR.
//
// Regression intent: before #413, gather-sibling-context emitted selectedNumber
// as a JSON int, so buildStageEnv dropped it and apply-verdict aborted with
// "selectedNumber is required" on every run — no eligible PR ever got a
// merge-review label. The older TestMergeReviewNamesCrossPRConflict missed this
// because it hardcoded GOOBERS_INPUT_SELECTEDNUMBER once (persisting via
// t.Setenv) instead of threading each hop from the prior stage's real output.
func TestMergeReviewThreadsSelectedNumberToApplyVerdict(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	const (
		prNumber = 10
		headSHA  = "sha10head"
		baseSHA  = "shamainbase"
	)
	// One eligible PR: open, non-draft, unlabeled — pr-select picks it.
	server.addIssue(prNumber, "Eligible PR")
	server.addOpenPR(prNumber, "goobers/implementation/run-10", "main", headSHA, baseSHA,
		false, nil, []fakePRFile{{path: "internal/foo.go", status: "modified", additions: 3, deletions: 1}})

	const runID = "run-1"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")

	// pr-select -> selected-pr.json.
	selectDir := t.TempDir()
	t.Chdir(selectDir)
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	// Thread pr-select's `number` into gather the way the runner does — not
	// hardcoded. (Guards that pr-select emits `number` as a string too.)
	selectedNum, threaded := threadOutput(t, filepath.Join(selectDir, "selected-pr.json"), "number")
	if !threaded {
		t.Fatal("pr-select's `number` did not thread to gather-sibling-context — it must be a string output")
	}
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", selectedNum)

	// gather-sibling-context -> sibling-context.json.
	gatherDir := t.TempDir()
	t.Chdir(gatherDir)
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	// The core #413 assertion: gather emits selectedNumber as a STRING, so it
	// threads to apply-verdict. A JSON int here (the bug) → threaded=false.
	gatherResult := filepath.Join(gatherDir, "sibling-context.json")
	selectedNumberForApply, threaded := threadOutput(t, gatherResult, "selectedNumber")
	if !threaded {
		t.Fatal("gather-sibling-context's selectedNumber did not thread to apply-verdict — " +
			"it must be emitted as a string (#413), else buildStageEnv drops it and apply-verdict fails 100%")
	}
	if selectedNumberForApply != "10" {
		t.Fatalf("threaded selectedNumber = %q, want \"10\"", selectedNumberForApply)
	}

	// The review gate approves (decision:pass) with a SHA pin matching the PR's
	// current state — the primary path that produced no label before #413.
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictPass,
		Summary:  "no cross-PR conflicts; ready to merge",
		HeadSHA:  headSHA,
		BaseSHA:  baseSHA,
	})

	// apply-verdict — selectedNumber comes from gather's real output (re-set,
	// overwriting pr-select's), the way the runner's InputsFrom threads it.
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", selectedNumberForApply)
	applyDir := t.TempDir()
	t.Chdir(applyDir)
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	// A native approval was actually published for the eligible PR.
	server.mu.Lock()
	issue := server.issues[prNumber]
	reviews := append([]fakeReview(nil), server.prs[prNumber].reviews...)
	server.mu.Unlock()
	if issue == nil {
		t.Fatal("selected PR's issue record vanished")
	}
	if len(reviews) != 1 || reviews[0].state != "APPROVED" || reviews[0].commitSHA != headSHA {
		t.Fatalf("native reviews = %+v, want one APPROVED review pinned to %s", reviews, headSHA)
	}
	if hasAnyLabel(issue.labels, []string{"goobers:merge-ready"}) {
		t.Fatalf("labels = %v, pass must stay unlabeled so a stale-dismissed approval can be re-reviewed", issue.labels)
	}
	if len(issue.comments) != 1 {
		t.Fatalf("comments = %v, want one pass verdict for merge and cache consumers", issue.comments)
	}
	posted, ok := parseVerdictComment(issue.comments[0])
	if !ok || posted.Decision != apiv1.VerdictPass || posted.HeadSHA != headSHA || posted.BaseSHA != baseSHA {
		t.Fatalf("posted verdict = %+v, ok = %v, want current pass verdict", posted, ok)
	}
	_, commitMessage, err := structuredMergeCommitMessage(providers.PullRequestPollResult{
		Title:         "Eligible PR",
		HeadSHA:       headSHA,
		BaseSHA:       baseSHA,
		CommentsSince: []providers.PullRequestComment{{Author: server.authenticatedLogin, Body: issue.comments[0]}},
	}, server.authenticatedLogin)
	if err != nil {
		t.Fatalf("structured merge message from posted pass verdict: %v", err)
	}
	if commitMessage != "no cross-PR conflicts; ready to merge" {
		t.Fatalf("commit message = %q, want posted pass summary", commitMessage)
	}
}
