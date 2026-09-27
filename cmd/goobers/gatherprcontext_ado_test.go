package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// TestGatherPRContextADOPopulatesVerdictFromThread is the ADO-branch counterpart
// of TestGatherPRContextChecksOutSelectedPRAndLoadsContext (remediation-wiring-plan
// §3.1). It proves gather-pr-context's ADO path: a PR carrying the
// goobers:needs-remediation PR label (the label tier, read straight off ADO's
// ListPullRequests) is selected, its branch is rebound into the run's worktree,
// and — the keystone assertion — the merge-review verdict is recovered from the
// PR THREAD (ListPullRequestThreadComments, the ADO analog of the GitHub PR-comment
// list) and populates the remediation brief's Verdict. It never resolves a
// github:* token and never runs the GitHub-concrete remediationProvider helpers.
func TestGatherPRContextADOPopulatesVerdictFromThread(t *testing.T) {
	const (
		prBranch = "goobers/impl/run-ado-362"
		prNumber = 359
		login    = "merge-review-bot"
	)
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	// The merge-review verdict apply-verdict posted to the PR thread: a
	// needs-changes verdict carrying a substantive finding attributed to this PR,
	// with the verdict-json machine payload renderVerdictComment embeds.
	verdictBody := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "Address one substantive finding.",
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Message:  "fix the off-by-one",
			Location: fmt.Sprintf("PR #%d", prNumber),
			Class:    apiv1.FindingSubstantive,
		}},
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
	})

	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)

	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	var threadsListed, connectionDataRead bool
	mux := http.NewServeMux()
	// The active-PR list carrying the needs-remediation PR label — the selector
	// signal the label tier fires on.
	mux.HandleFunc(prBase, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{
			map[string]interface{}{
				"pullRequestId":         prNumber,
				"status":                "active",
				"sourceRefName":         "refs/heads/" + prBranch,
				"targetRefName":         "refs/heads/main",
				"isDraft":               false,
				"createdBy":             map[string]string{"displayName": "goober", "uniqueName": "goober@example.com"},
				"labels":                []map[string]string{{"name": needsRemediationLabel}},
				"lastMergeSourceCommit": map[string]string{"commitId": headSHA},
				"lastMergeTargetCommit": map[string]string{"commitId": baseSHA},
				"_links":                map[string]interface{}{"web": map[string]string{"href": "https://dev.azure.test/pr/359"}},
			},
		}})
	})
	// The PR thread carrying the merge-review verdict — the keystone read that
	// replaces GitHub's issue-comment ListComments (ado_prthreads.go).
	mux.HandleFunc(prBase+"/"+strconv.Itoa(prNumber)+"/threads", func(w http.ResponseWriter, _ *http.Request) {
		threadsListed = true
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{
			// A system thread (vote/status event) that must be skipped.
			map[string]interface{}{
				"id":     10,
				"status": "active",
				"comments": []map[string]interface{}{{
					"id": 1, "content": "voted 10", "commentType": "system",
					"author": map[string]string{"displayName": login}, "publishedDate": "2026-08-08T00:00:00Z",
				}},
			},
			// The real verdict thread from the trusted author.
			map[string]interface{}{
				"id":     11,
				"status": "active",
				"comments": []map[string]interface{}{{
					"id": 2, "content": verdictBody, "commentType": "text",
					"author": map[string]string{"displayName": login}, "publishedDate": "2026-08-08T00:01:00Z",
				}},
			},
		}})
	})
	// PR detail + policy evaluations: the selected PR's CI state (ADO-N22). A
	// rejected blocking build policy must surface as hasFailingCI.
	handleADOSelectedPRRejectedBuild(t, mux, repo, prNumber, headSHA, baseSHA)
	// connectionData: the authenticated identity whose displayName matches the
	// thread author, so gatherPRVerdict's trusted-author filter recognizes the
	// thread we posted.
	mux.HandleFunc("/"+repo.Owner+"/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		connectionDataRead = true
		writeJSONResp(t, w, map[string]interface{}{
			"authenticatedUser": map[string]string{"id": "merge-review-bot-guid", "providerDisplayName": login},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	original := newADOProviderForStage
	newADOProviderForStage = func(routed providers.RepositoryRef, _ providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-ado-362", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-ado-362",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	t.Setenv("GOOBERS_RUN_ID", "run-ado-362")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	// Every ADO auth kind delivers the declared capabilities' credentials: the
	// provider authenticates with github:pr:write and Git with repo:push.
	setDeliveredADOStageCredentials(t)
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", root)
	if code != 0 {
		t.Fatalf("gather-pr-context (ADO): code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !threadsListed {
		t.Fatal("PR threads were never listed — the verdict was not recovered from the PR thread")
	}
	if !connectionDataRead {
		t.Fatal("connectionData (AuthenticatedLogin) was never read — the thread author could not be trusted")
	}
	if !strings.Contains(stdout, "PR #359") {
		t.Fatalf("stdout = %q, want a mention of PR #359", stdout)
	}

	// The branch was rebound to the PR's own branch (#392), not left on the
	// runner's default pr-remediation branch.
	branch := strings.TrimSpace(runGitOutputT(t, wt.Path, "symbolic-ref", "--short", "HEAD"))
	if branch != prBranch {
		t.Fatalf("checked-out branch = %q, want %q (the PR's own branch)", branch, prBranch)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read %s: %v", remediationBriefResultFile, err)
	}
	var got apiv1.RemediationBrief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal %s: %v (data=%s)", remediationBriefResultFile, err, data)
	}
	if got.SelectedNumber != "359" || got.Head != prBranch {
		t.Fatalf("got = %+v, want selectedNumber=\"359\" head=%q", got, prBranch)
	}
	if got.GatherPRContext.HeadSHA != headSHA || got.GatherPRContext.BaseSHA != baseSHA {
		t.Fatalf("gatherPrContext SHA pins = %q/%q, want %q/%q",
			got.GatherPRContext.HeadSHA, got.GatherPRContext.BaseSHA, headSHA, baseSHA)
	}
	// The keystone: the verdict was recovered from the PR thread and populated.
	verdict := got.GatherPRContext.Verdict
	if verdict == nil {
		t.Fatal("brief Verdict is nil — the merge-review verdict was not recovered from the PR thread")
	}
	if verdict.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("verdict.Decision = %q, want needs-changes (recovered from the seeded thread)", verdict.Decision)
	}
	if len(verdict.Findings) != 1 || verdict.Findings[0].Class != apiv1.FindingSubstantive {
		t.Fatalf("verdict.Findings = %+v, want the one substantive finding from the thread", verdict.Findings)
	}
	if got.HasSubstantiveFindings != "true" {
		t.Fatalf("hasSubstantiveFindings = %q, want \"true\" (the thread verdict names this PR)", got.HasSubstantiveFindings)
	}
	// The system thread is skipped; only the real verdict comment surfaces.
	if len(got.GatherPRContext.Comments) != 1 {
		t.Fatalf("comments = %+v, want only the non-system verdict thread comment", got.GatherPRContext.Comments)
	}
	// ADO-N22: the rejected blocking build policy makes the selected PR's CI
	// failing, so the brief routes to gather-ci-failures ...
	if got.HasFailingCI != "true" {
		t.Fatalf("hasFailingCI = %q, want \"true\" (a blocking build policy is rejected)", got.HasFailingCI)
	}
	// ... which, fed this very brief, records the rejected policy as evidence.
	ci := runGatherCIFailuresOnBrief(t, root, "run-ado-362-ci", got)
	if ci.GatherCIFailures == nil || len(ci.GatherCIFailures.Checks) != 1 || ci.GatherCIFailures.Checks[0].Conclusion != "rejected" {
		t.Fatalf("CI failures = %#v, want the rejected build policy", ci.GatherCIFailures)
	}
}

// handleADOSelectedPRRejectedBuild serves the selected PR's detail and its
// policy evaluations: one rejected blocking build policy and a queued
// reviewer policy, which is a human wait and never CI.
func handleADOSelectedPRRejectedBuild(t *testing.T, mux *http.ServeMux, repo providers.RepositoryRef, prNumber int, headSHA, baseSHA string) {
	t.Helper()
	build := adoBlockingPolicyEvaluation(adoTestPolicyTypeBuild, "Build", "rejected")
	build["context"] = map[string]interface{}{"buildId": 314}
	handleADOSelectedPRPolicies(t, mux, repo, prNumber, headSHA, baseSHA, build,
		adoBlockingPolicyEvaluation(adoTestPolicyTypeMinimumReviewers, "Minimum number of reviewers", "queued"))
}

// Azure DevOps policy type ids the selected-PR CI-state fixtures use.
const (
	adoTestPolicyTypeBuild               = "0609b952-1397-4640-95ec-e00a01b2c241"
	adoTestPolicyTypeStatus              = "cbdc66da-9728-4af8-aada-9a5a32e4a226"
	adoTestPolicyTypeMinimumReviewers    = "fa4e907d-c16b-4a4c-9dfa-4906e5d171dd"
	adoTestPolicyTypeCommentRequirements = "c6a1889d-b943-4856-b76f-9e46bb6b0df2"
	adoTestPolicyTypeWorkItemLinking     = "40e92b44-2fe1-4dd6-b3d8-74a9c21d0c6e"
)

// adoBlockingPolicyEvaluation is one enabled, blocking policy evaluation of
// the given policy type in the given status.
func adoBlockingPolicyEvaluation(typeID, name, status string) map[string]interface{} {
	return map[string]interface{}{"status": status, "configuration": map[string]interface{}{
		"id": 5, "isEnabled": true, "isBlocking": true,
		"type": map[string]string{"id": typeID, "displayName": name},
	}}
}

// handleADOSelectedPRPolicies serves the selected PR's detail and the given
// policy evaluations for it.
func handleADOSelectedPRPolicies(t *testing.T, mux *http.ServeMux, repo providers.RepositoryRef, prNumber int, headSHA, baseSHA string, evaluations ...map[string]interface{}) {
	t.Helper()
	prPath := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests/" + strconv.Itoa(prNumber)
	mux.HandleFunc(prPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("PR detail request method = %s, want GET", r.Method)
		}
		writeJSONResp(t, w, map[string]interface{}{
			"pullRequestId": prNumber, "status": "active", "title": "t",
			"sourceRefName": "refs/heads/goobers/impl/run-ado-362", "targetRefName": "refs/heads/main",
			"lastMergeSourceCommit": map[string]string{"commitId": headSHA},
			"lastMergeTargetCommit": map[string]string{"commitId": baseSHA},
			"repository": map[string]interface{}{
				"id": "repo-guid", "name": repo.Name,
				"project": map[string]string{"id": "proj-guid", "name": repo.Project},
			},
		})
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": evaluations})
	})
}

// TestResolveADOSelectedCheckStateCountsOnlyCIPolicies pins that the selected
// ADO pull request's CI state, and so hasFailingCI, follows exactly the
// evidence gather-ci-failures reports (ADO-N22): a rejected build or broken
// status policy fails CI, while rejected comment-resolution, work-item-linking
// or reviewer policies are human waits that leave hasFailingCI false.
func TestResolveADOSelectedCheckStateCountsOnlyCIPolicies(t *testing.T) {
	const prNumber = 359
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "example-project", Name: "example-repo"}
	cases := []struct {
		name        string
		evaluations []map[string]interface{}
		want        string
	}{
		{"human-policies-only", []map[string]interface{}{
			adoBlockingPolicyEvaluation(adoTestPolicyTypeCommentRequirements, "Comment requirements", "rejected"),
			adoBlockingPolicyEvaluation(adoTestPolicyTypeWorkItemLinking, "Work item linking", "rejected"),
			adoBlockingPolicyEvaluation(adoTestPolicyTypeMinimumReviewers, "Minimum number of reviewers", "rejected"),
		}, "false"},
		{"rejected-build", []map[string]interface{}{
			adoBlockingPolicyEvaluation(adoTestPolicyTypeBuild, "Build", "rejected"),
			adoBlockingPolicyEvaluation(adoTestPolicyTypeWorkItemLinking, "Work item linking", "rejected"),
		}, "true"},
		{"broken-status", []map[string]interface{}{
			adoBlockingPolicyEvaluation(adoTestPolicyTypeStatus, "Status", "broken"),
		}, "true"},
		{"passing-build", []map[string]interface{}{
			adoBlockingPolicyEvaluation(adoTestPolicyTypeBuild, "Build", "approved"),
		}, "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			handleADOSelectedPRPolicies(t, mux, repo, prNumber, "headsha", "basesha", tc.evaluations...)
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			provider := providers.NewADOProvider(repo.Owner, repo.Project, "token",
				func(p *providers.ADOProvider) { p.BaseURL = server.URL })
			pr := providers.PullRequestSummary{Number: prNumber, CheckState: providers.CheckStatePending}
			if err := resolveADOSelectedCheckState(t.Context(), provider, repo, &pr); err != nil {
				t.Fatalf("resolveADOSelectedCheckState: %v", err)
			}
			// gather-pr-context's hasFailingCI is exactly this comparison.
			if got := strconv.FormatBool(pr.CheckState == providers.CheckStateFailing); got != tc.want {
				t.Fatalf("hasFailingCI = %s (check state %q), want %s", got, pr.CheckState, tc.want)
			}
		})
	}
}

func TestGatherPRContextADOParksRepeatedEscalatedDigest(t *testing.T) {
	const (
		prNumber = 359
		prBranch = "goobers/impl/run-359"
	)
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)
	root, repo := providerDispatchFixture(t, providers.ProviderADO)

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-ado-digest", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-ado-digest",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })
	if _, err := checkoutExistingBranchWithAuth(t.Context(), wt.Path, prBranch, tokenGitAuthEnvironment("test-token")); err != nil {
		t.Fatalf("checkout PR branch: %v", err)
	}
	digest, err := diffDigest(wt.Path, baseSHA)
	if err != nil {
		t.Fatalf("diffDigest: %v", err)
	}
	stateBody, err := remediationStateComment(remediationState{
		Cycles: 1, LastDiffDigest: digest, HeadSHA: headSHA, BaseSHA: baseSHA,
		Escalated: true, EscalatedHeadSHA: headSHA, EscalatedBaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	threadValues := []interface{}{map[string]interface{}{
		"id": 17, "status": "active",
		"comments": []interface{}{map[string]interface{}{
			"id": 23, "content": stateBody, "commentType": "text",
			"author":        map[string]string{"displayName": "merge-review-bot"},
			"publishedDate": "2026-08-08T00:01:00Z",
		}},
	}}
	rec := &adoCheckpointRecorder{}
	mux := adoCheckpointMux(t, repo, prNumber, headSHA, baseSHA, []string{needsRemediationLabel}, threadValues, rec)
	mux.HandleFunc("/"+repo.Owner+"/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"authenticatedUser": map[string]string{"id": "merge-review-bot-guid", "providerDisplayName": "merge-review-bot"},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stubADOProviderForCheckpointStage(t, server.URL)

	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_RUN_ID", "run-ado-digest")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)
	resultFile := filepath.Join(wt.Path, remediationBriefResultFile)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	seedRemediationNoopState(
		t,
		layoutFor(root),
		remediationNoopKey("", prNumber),
		remediationNoopSignature{HeadSHA: headSHA, DiffDigest: digest},
		"prior-ado-digest-run",
	)

	code, stdout, stderr := runArgs(t, "gather-pr-context", root)
	if code != 0 {
		t.Fatalf("gather-pr-context (ADO): code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "visibly parked") {
		t.Fatalf("stdout = %q, want visible parking result", stdout)
	}
	assertNoWorkProviderStageResult(t, resultFile)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !hasAnyLabel(rec.addedLabels, []string{remediationEscalatedLabel}) {
		t.Fatalf("added labels = %v, want %s", rec.addedLabels, remediationEscalatedLabel)
	}
	if !hasAnyLabel(rec.removedLabels, []string{needsRemediationLabel}) {
		t.Fatalf("removed labels = %v, want %s", rec.removedLabels, needsRemediationLabel)
	}
	if !rec.threadPatched || !strings.Contains(rec.patchedContent, "unchanged diff digest") {
		t.Fatalf("thread patched = %v, content = %q; want visible unchanged-digest reason", rec.threadPatched, rec.patchedContent)
	}
	if rec.workItemTouched {
		t.Fatal("ADO parking touched a work item instead of native PR labels")
	}
	record := remediationNoopStateRecord(t, layoutFor(root), remediationNoopKey("", prNumber))
	if record.Attempts != remediationNoopLimit || !record.Parked {
		t.Fatalf("no-op record = %+v, want parked at limit %d", record, remediationNoopLimit)
	}
}
