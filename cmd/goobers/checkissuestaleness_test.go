package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
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
	pin := formatIssueSpecPin("901", snapshotAt.Format(time.RFC3339), "Original issue", "Original body")
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

// TestCheckIssueStalenessAdvancesPinSoARepeatRunDoesNotReFire is the
// regression test for the live-lock this stage used to cause indefinitely
// (observed on #2384): once a PR is routed to remediation for a stale issue
// edit, nothing else ever rewrote the pin, so every subsequent run of this
// same stage re-compared against the original implementation-time snapshot
// and re-reported the identical already-handled edit forever, even though
// the issue itself had gone quiet. The stage must advance its own pin when
// it reports staleness, so a rerun against an unchanged issue is not stale.
func TestCheckIssueStalenessAdvancesPinSoARepeatRunDoesNotReFire(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(54, "pr 54 placeholder")
	server.addOpenPR(54, "goobers/implementation/run-54", "main", "head-54", "base-54", false, nil, nil)
	server.addIssue(903, "Corrected issue")

	snapshotAt := time.Now().UTC().Add(-2 * time.Hour)
	editedAt := time.Now().UTC().Truncate(time.Second)
	server.setIssueUpdatedAt(903, editedAt)
	pin := formatIssueSpecPin("903", snapshotAt.Format(time.RFC3339), "Original issue", "Original body")
	server.setPRBody(54, "Implements #903: **Corrected issue**.\n\n---\nFixes #903\n"+pin)

	root, dir := checkIssueStalenessEnv(t, server, "54")
	if code, stdout, stderr := runArgs(t, "check-issue-staleness", root); code != 0 {
		t.Fatalf("first check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	first := readIssueStalenessResult(t, dir)
	if first["issueStale"] != "true" {
		t.Fatalf("first result = %+v, want issueStale=true", first)
	}

	server.mu.Lock()
	refreshedBody := server.prs[54].body
	commentsAfterFirst := len(server.issues[54].comments)
	server.mu.Unlock()
	refreshedPin, ok := parseIssueSpecPin(refreshedBody)
	if !ok {
		t.Fatalf("pr body after first run has no pin at all:\n%s", refreshedBody)
	}
	if refreshedPin.IssueID != "903" {
		t.Fatalf("refreshed pin issueId = %q, want 903", refreshedPin.IssueID)
	}
	if refreshedPin.UpdatedAt != editedAt.Format(time.RFC3339) {
		t.Fatalf("refreshed pin updatedAt = %q, want %q (the edit just observed, not the original snapshot)",
			refreshedPin.UpdatedAt, editedAt.Format(time.RFC3339))
	}

	// Rerun against the same (unchanged since the first run) issue state --
	// the regression this guards against is this second run re-reporting the
	// exact same edit because the pin never advanced.
	dir2 := t.TempDir()
	t.Chdir(dir2)
	if code, stdout, stderr := runArgs(t, "check-issue-staleness", root); code != 0 {
		t.Fatalf("second check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	second := readIssueStalenessResult(t, dir2)
	if second["issueStale"] != "false" {
		t.Fatalf("second result = %+v, want issueStale=false now that the pin has advanced past this edit", second)
	}

	server.mu.Lock()
	commentsAfterSecond := len(server.issues[54].comments)
	server.mu.Unlock()
	if commentsAfterSecond != commentsAfterFirst {
		t.Fatalf("second run posted %d more comment(s); want no new staleness comment for an edit already reported",
			commentsAfterSecond-commentsAfterFirst)
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
	pin := formatIssueSpecPin("902", snapshotAt.Format(time.RFC3339), "Untouched issue", "")
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

func TestCheckIssueStalenessIgnoresCommentOnlyTimestampChange(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(55, "pr 55 placeholder")
	server.addOpenPR(55, "goobers/implementation/run-55", "main", "head-55", "base-55", false, nil, nil)
	server.addIssue(904, "Untouched issue")

	snapshotAt := time.Now().UTC().Add(-2 * time.Hour)
	server.setIssueUpdatedAt(904, time.Now().UTC())
	pin := formatIssueSpecPin("904", snapshotAt.Format(time.RFC3339), "Untouched issue", "")
	server.setPRBody(55, "Implements #904: **Untouched issue**.\n\n---\nFixes #904\n"+pin)

	root, dir := checkIssueStalenessEnv(t, server, "55")
	if code, stdout, stderr := runArgs(t, "check-issue-staleness", root); code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "false" {
		t.Fatalf("result = %+v, want comment-only updatedAt change ignored", result)
	}
	server.mu.Lock()
	labels := append([]string(nil), server.issues[55].labels...)
	comments := append([]string(nil), server.issues[55].comments...)
	server.mu.Unlock()
	if hasAnyLabel(labels, []string{needsRemediationLabel}) || len(comments) != 0 {
		t.Fatalf("PR labels/comments = %v/%v, want no remediation for comment-only activity", labels, comments)
	}
}

func TestCheckIssueStalenessLegacyTimestampPinStillRoutes(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(56, "pr 56 placeholder")
	server.addOpenPR(56, "goobers/implementation/run-56", "main", "head-56", "base-56", false, nil, nil)
	server.addIssue(905, "Legacy pinned issue")

	snapshotAt := time.Now().UTC().Add(-2 * time.Hour)
	server.setIssueUpdatedAt(905, time.Now().UTC())
	pin := `<!-- issue-spec-pin: {"issueId":"905","updatedAt":"` + snapshotAt.Format(time.RFC3339) + `"} -->`
	server.setPRBody(56, "Implements #905.\n\n---\nFixes #905\n"+pin)

	root, dir := checkIssueStalenessEnv(t, server, "56")
	if code, stdout, stderr := runArgs(t, "check-issue-staleness", root); code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "true" {
		t.Fatalf("result = %+v, want legacy timestamp-only pin to retain prior behavior", result)
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
	pin := formatIssueSpecPin("999999", time.Now().UTC().Format(time.RFC3339), "Deleted issue", "")
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

// TestCheckIssueStalenessADODispatchesAndDetectsStale is the ADO execution path
// test for check-issue-staleness: it proves runCheckIssueStaleness routes through
// the ADO provider (not GitHub), reads work items from the backlog project (via
// backlogRepoRefForStage routing when GOOBERS_GAGGLE is set), detects stale work
// items correctly, and correctly skips the GitHub-specific remediation write that
// would cause the PR-as-work-item wrong-object hazard on ADO.
func TestCheckIssueStalenessADODispatchesAndDetectsStale(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_INPUT_PULLNUMBER", "360")

	snapshotAt := time.Now().UTC().Add(-2 * time.Hour)
	updatedAfterSnapshot := time.Now().UTC()
	pin := `<!-- issue-spec-pin: {"issueId":"1457","updatedAt":"` + snapshotAt.Format(time.RFC3339) + `"} -->`

	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	mux := http.NewServeMux()

	mux.HandleFunc(prBase+"/360", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"pullRequestId":         360,
			"status":                "active",
			"description":           "Implements PBI 1457\n\nFixes #1457\n" + pin,
			"sourceRefName":         "refs/heads/goobers/tb-ado-implementation/run-360",
			"targetRefName":         "refs/heads/main",
			"lastMergeSourceCommit": map[string]string{"commitId": "head-sha"},
			"lastMergeTargetCommit": map[string]string{"commitId": "base-sha"},
			"repository": map[string]interface{}{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})

	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitemtypes/Issue/states", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"value": []map[string]interface{}{
				{
					"name":     "Active",
					"category": "Proposed",
				},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitems/1457", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSONResp(t, w, map[string]interface{}{
				"id":  1457,
				"rev": 1,
				"url": "https://dev.azure.com/acme/project/_apis/wit/workitems/1457",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.Title":        "Updated title",
					"System.Description":  "Updated body",
					"System.ChangedDate":  updatedAfterSnapshot.Format(time.RFC3339Nano),
					"System.State":        "Active",
				},
			})
		} else {
			t.Errorf("unexpected method %s on workitems/1457", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})

	// The PR-as-work-item write would PATCH wit/workitems/360 — it must never be reached on ADO.
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitems/360", func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("wit/workitems/360 %s — check-issue-staleness ran the PR-as-work-item write on ADO when stale", r.Method)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	dir := t.TempDir()
	t.Chdir(dir)
	code, stdout, stderr := runArgs(t, "check-issue-staleness", root)
	if code != 0 {
		t.Fatalf("check-issue-staleness: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	result := readIssueStalenessResult(t, dir)
	if result["issueStale"] != "true" {
		t.Fatalf("result = %+v, want issueStale=true when work item is stale on ADO", result)
	}
	if result["number"] != "360" {
		t.Fatalf("result = %+v, want number=360", result)
	}
	reason, ok := result["reason"].(string)
	if !ok || !strings.Contains(reason, "1457") {
		t.Fatalf("reason = %q, want it to mention work item #1457", reason)
	}
	if !strings.Contains(stdout, "reporting issueStale without mutation") {
		t.Fatalf("stdout = %q, want warning about ADO remediation write being skipped", stdout)
	}
}
