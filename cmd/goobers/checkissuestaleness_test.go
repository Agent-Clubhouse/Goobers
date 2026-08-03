package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkIssueStalenessEnv(t *testing.T, server *fakeGitHubServer, pullNumber string) (root, dir string) {
	t.Helper()
	root = initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-check-staleness")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_PULLNUMBER", pullNumber)
	dir = t.TempDir()
	t.Chdir(dir)
	return root, dir
}

func readIssueStalenessResult(t *testing.T, dir string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "issue-staleness-result.json"))
	if err != nil {
		t.Fatalf("read issue-staleness-result.json: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal issue-staleness-result.json: %v", err)
	}
	return out
}

// TestCheckIssueStalenessNoPinIsNeverStale is #2340's compatibility floor: a
// PR predating this feature (or whose implementation run never resolved the
// issue's updatedAt) carries no issue-spec-pin marker at all. There is
// nothing to compare against, so it must never be treated as stale.
func TestCheckIssueStalenessNoPinIsNeverStale(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(50, "pr 50 placeholder")
	server.addOpenPR(50, "goobers/implementation/run-50", "main", "head-50", "base-50", false, nil, nil)
	server.setPRBody(50, "Implements #900: **Some issue**.\n\n---\nFixes #900")

	root, dir := checkIssueStalenessEnv(t, server, "50")
	code, stdout, stderr := runArgs(t, "check-issue-staleness", root)
	if code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "false" {
		t.Fatalf("result = %+v, want issueStale=false", result)
	}
	server.mu.Lock()
	labels := append([]string(nil), server.issues[50].labels...)
	server.mu.Unlock()
	if hasAnyLabel(labels, []string{needsRemediationLabel}) {
		t.Fatalf("pr labels = %v, want no %s label added", labels, needsRemediationLabel)
	}
}

// TestCheckIssueStalenessRoutesToRemediationWhenIssueChangedSince is #2340's
// core acceptance criterion: the linked issue was edited after the
// implementation-time snapshot, so the PR must be routed to remediation
// instead of reviewed against the stale copied criteria.
func TestCheckIssueStalenessRoutesToRemediationWhenIssueChangedSince(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(50, "pr 50 placeholder") // so UpdateWorkItem/comment on PR #50 resolves
	server.addOpenPR(50, "goobers/implementation/run-50", "main", "head-50", "base-50", false, nil, nil)
	server.addIssue(901, "Corrected issue")

	snapshotAt := time.Now().UTC().Add(-2 * time.Hour)
	server.setIssueUpdatedAt(901, time.Now().UTC()) // "now" is after snapshotAt
	pin := formatIssueSpecPin("901", snapshotAt.Format(time.RFC3339))
	server.setPRBody(50, "Implements #901: **Corrected issue**.\n\n---\nFixes #901\n"+pin)

	root, dir := checkIssueStalenessEnv(t, server, "50")
	code, stdout, stderr := runArgs(t, "check-issue-staleness", root)
	if code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "true" {
		t.Fatalf("result = %+v, want issueStale=true", result)
	}
	if reason, _ := result["reason"].(string); !strings.Contains(reason, "901") {
		t.Fatalf("reason = %q, want it to mention issue #901", reason)
	}

	server.mu.Lock()
	labels := append([]string(nil), server.issues[50].labels...)
	comments := append([]string(nil), server.issues[50].comments...)
	server.mu.Unlock()
	if !hasAnyLabel(labels, []string{needsRemediationLabel}) {
		t.Fatalf("pr labels = %v, want %s", labels, needsRemediationLabel)
	}
	if len(comments) == 0 || !strings.Contains(comments[len(comments)-1], "Issue spec changed") {
		t.Fatalf("pr comments = %v, want an explanatory staleness comment", comments)
	}
}

// TestCheckIssueStalenessNotStaleWhenIssueUnchangedSinceSnapshot confirms the
// negative case pairs with the positive one above: the pin still matches the
// issue's live updatedAt (no edit since implementation began), so review must
// proceed normally.
func TestCheckIssueStalenessNotStaleWhenIssueUnchangedSinceSnapshot(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(51, "pr 51 placeholder")
	server.addOpenPR(51, "goobers/implementation/run-51", "main", "head-51", "base-51", false, nil, nil)
	server.addIssue(902, "Untouched issue")

	// Truncated to whole seconds: real GitHub updatedAt timestamps carry no
	// sub-second precision, and the pin round-trips through time.RFC3339
	// (also whole-second) — an untruncated time.Now() would spuriously
	// compare "after" its own truncated pin.
	snapshotAt := time.Now().UTC().Truncate(time.Second)
	server.setIssueUpdatedAt(902, snapshotAt) // unchanged since the snapshot
	pin := formatIssueSpecPin("902", snapshotAt.Format(time.RFC3339))
	server.setPRBody(51, "Implements #902: **Untouched issue**.\n\n---\nFixes #902\n"+pin)

	root, dir := checkIssueStalenessEnv(t, server, "51")
	code, stdout, stderr := runArgs(t, "check-issue-staleness", root)
	if code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "false" {
		t.Fatalf("result = %+v, want issueStale=false", result)
	}
}

// TestCheckIssueStalenessPinnedIssueGoneIsNotStale mirrors
// gather-issue-context's tolerant handling of a since-deleted issue: there is
// nothing left to compare against, so the PR fails open rather than getting
// stuck unable to ever pass this gate.
func TestCheckIssueStalenessPinnedIssueGoneIsNotStale(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(52, "pr 52 placeholder")
	server.addOpenPR(52, "goobers/implementation/run-52", "main", "head-52", "base-52", false, nil, nil)
	pin := formatIssueSpecPin("999999", time.Now().UTC().Format(time.RFC3339))
	server.setPRBody(52, "Implements #999999.\n\n---\nFixes #999999\n"+pin)

	root, dir := checkIssueStalenessEnv(t, server, "52")
	code, stdout, stderr := runArgs(t, "check-issue-staleness", root)
	if code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "false" {
		t.Fatalf("result = %+v, want issueStale=false for an unresolvable pinned issue", result)
	}
}

// TestCheckIssueStalenessPassesThroughSelectionFields confirms the stage
// re-emits pr-select's number/head/base/advisoryMode unchanged, since
// gather-sibling-context's inputsFrom resolves against whichever task
// immediately precedes it in the chain (now this stage, not pr-select).
func TestCheckIssueStalenessPassesThroughSelectionFields(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(53, "pr 53 placeholder")
	server.addOpenPR(53, "goobers/implementation/run-53", "main", "head-53", "base-53", false, nil, nil)
	server.setPRBody(53, "no pin here")

	root, dir := checkIssueStalenessEnv(t, server, "53")
	t.Setenv("GOOBERS_INPUT_HEAD", "goobers/implementation/run-53")
	t.Setenv("GOOBERS_INPUT_BASE", "main")
	t.Setenv("GOOBERS_INPUT_ADVISORYMODE", "true")
	if code, stdout, stderr := runArgs(t, "check-issue-staleness", root); code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["number"] != "53" || result["head"] != "goobers/implementation/run-53" ||
		result["base"] != "main" || result["advisoryMode"] != "true" {
		t.Fatalf("result = %+v, want passthrough of pr-select's fields", result)
	}
}
