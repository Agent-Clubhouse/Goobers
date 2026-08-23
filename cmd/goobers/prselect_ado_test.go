package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// fakeADOSelectProvider is a minimal adoSelectProvider for pr-select's Azure
// DevOps branch: it serves a fixed active-PR list plus a per-PR poll result so a
// test can assert the branch resolves each candidate's CheckState and its
// open/merged State from PollPullRequest — the branch-policy source of truth —
// never from RefCheckState/GetPullRequest, which ADO does not implement
// (merge-wiring-plan §2).
type fakeADOSelectProvider struct {
	open      []providers.PullRequestSummary
	polls     map[int]providers.PullRequestPollResult
	polledIDs []string
	listBase  string
}

func (f *fakeADOSelectProvider) ListPullRequests(_ context.Context, req providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
	f.listBase = req.Base
	return append([]providers.PullRequestSummary(nil), f.open...), nil
}

func (f *fakeADOSelectProvider) PollPullRequest(_ context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	f.polledIDs = append(f.polledIDs, req.PullID)
	n, _ := strconv.Atoi(req.PullID)
	poll, ok := f.polls[n]
	if !ok {
		return providers.PullRequestPollResult{}, fmt.Errorf("no poll fixture for PR %s", req.PullID)
	}
	return poll, nil
}

// TestPullRequestsForSelectionADOResolvesCheckStateFromPoll proves the list path
// (merge-wiring-plan §1b/§2): ADO's ListPullRequests leaves each summary's State
// empty and CheckState pending, so pr-select's ADO branch must overlay both from
// PollPullRequest's policy evaluations. A policy-approved PR becomes
// passing+open; a policy-rejected PR becomes failing.
func TestPullRequestsForSelectionADOResolvesCheckStateFromPoll(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "project", Name: "web"}
	fake := &fakeADOSelectProvider{
		open: []providers.PullRequestSummary{
			// Shape ADO's ListPullRequests actually returns: State unset,
			// CheckState pending.
			{Number: 359, Head: "goobers/tb-ado-implementation/1456", Base: "main", HeadSHA: "h359", CheckState: providers.CheckStatePending},
			{Number: 360, Head: "goobers/tb-ado-implementation/1457", Base: "main", HeadSHA: "h360", CheckState: providers.CheckStatePending},
		},
		polls: map[int]providers.PullRequestPollResult{
			359: {Number: 359, State: "open", CheckState: providers.CheckStatePassing},
			360: {Number: 360, State: "open", CheckState: providers.CheckStateFailing},
		},
	}

	prs, openPRs, err := pullRequestsForSelectionADO(
		context.Background(), fake, repo, "main",
		[]string{"goobers/tb-ado-implementation/"}, authorScopeGoobers,
		providers.ListPullRequestsRequest{}, "", prSelectCompleteSnapshot, "",
	)
	if err != nil {
		t.Fatalf("pullRequestsForSelectionADO: %v", err)
	}
	if fake.listBase != "main" {
		t.Fatalf("list base = %q, want main", fake.listBase)
	}
	if len(openPRs) != 2 {
		t.Fatalf("openPRs = %d, want 2", len(openPRs))
	}
	if len(prs) != 2 {
		t.Fatalf("prs = %d, want 2", len(prs))
	}
	byNum := map[int]providers.PullRequestSummary{}
	for _, pr := range prs {
		byNum[pr.Number] = pr
	}
	if got := byNum[359]; got.CheckState != providers.CheckStatePassing || got.State != "open" {
		t.Fatalf("PR 359 = {CheckState:%q State:%q}, want {passing open} (resolved from PollPullRequest)", got.CheckState, got.State)
	}
	if got := byNum[360]; got.CheckState != providers.CheckStateFailing || got.State != "open" {
		t.Fatalf("PR 360 = {CheckState:%q State:%q}, want {failing open}", got.CheckState, got.State)
	}
	if len(fake.polledIDs) != 2 {
		t.Fatalf("polled IDs = %v, want each candidate polled once", fake.polledIDs)
	}
}

// TestPullRequestsForSelectionADOTargetedUsesPoll proves the webhook-targeted
// path (merge-wiring-plan §1b/§2) resolves the single targeted PR via
// PollPullRequest rather than GetPullRequest (absent on ADO). The returned
// summary is assembled entirely from the poll result, CheckState included.
func TestPullRequestsForSelectionADOTargetedUsesPoll(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "project", Name: "web"}
	fake := &fakeADOSelectProvider{
		// A non-empty list the targeted path must NOT select from.
		open: []providers.PullRequestSummary{
			{Number: 400, Head: "goobers/tb-ado-implementation/9", Base: "main"},
		},
		polls: map[int]providers.PullRequestPollResult{
			359: {
				Number: 359, State: "open", CheckState: providers.CheckStatePassing,
				HeadBranch: "goobers/tb-ado-implementation/1456", BaseBranch: "main",
				HeadSHA: "h359", Author: "goobers-bot", URL: "https://dev.azure.test/pr/359",
			},
		},
	}

	prs, _, err := pullRequestsForSelectionADO(
		context.Background(), fake, repo, "main",
		[]string{"goobers/tb-ado-implementation/"}, authorScopeGoobers,
		providers.ListPullRequestsRequest{}, "github-webhook:pull_request#359", prSelectPartialSnapshot, "",
	)
	if err != nil {
		t.Fatalf("pullRequestsForSelectionADO: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 359 {
		t.Fatalf("prs = %+v, want exactly the targeted PR 359", prs)
	}
	if prs[0].CheckState != providers.CheckStatePassing || prs[0].State != "open" || prs[0].Head != "goobers/tb-ado-implementation/1456" {
		t.Fatalf("targeted PR summary = %+v, want it assembled from the poll result", prs[0])
	}
	polledTarget := false
	for _, id := range fake.polledIDs {
		if id == "359" {
			polledTarget = true
		}
	}
	if !polledTarget {
		t.Fatalf("polled IDs = %v, want the targeted PR resolved via PollPullRequest", fake.polledIDs)
	}
}

// adoPRSelectServer stands up a fake Azure DevOps REST surface for pr-select's
// end-to-end ADO path: the active-PR list, the single-PR detail, and one
// enabled+blocking Build policy evaluation with the given status. An "approved"
// evaluation reduces to CheckState=passing (ado_pullrequests.go
// pollPullRequestPolicies).
func adoPRSelectServer(t *testing.T, policyStatus string) *httptest.Server {
	t.Helper()
	const prJSON = `{
		"pullRequestId": 359,
		"status": "active",
		"title": "ADO PR 359",
		"description": "Fixes #1456",
		"sourceRefName": "refs/heads/goobers/tb-ado-implementation/1456",
		"targetRefName": "refs/heads/main",
		"isDraft": false,
		"createdBy": {"uniqueName": "goobers-bot"},
		"lastMergeSourceCommit": {"commitId": "h359"},
		"lastMergeTargetCommit": {"commitId": "base359"},
		"repository": {"id": "repo-guid", "name": "web", "project": {"id": "proj-guid", "name": "project"}},
		"_links": {"web": {"href": "https://dev.azure.test/pr/359"}}
	}`
	mux := http.NewServeMux()
	mux.HandleFunc("/acme/project/_apis/git/repositories/web/pullrequests", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"value": [%s]}`, prJSON)
	})
	mux.HandleFunc("/acme/project/_apis/git/repositories/web/pullrequests/359", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(prJSON))
	})
	mux.HandleFunc("/acme/project/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []map[string]any{{
				"status": policyStatus,
				"configuration": map[string]any{
					"id":         1,
					"isEnabled":  true,
					"isBlocking": true,
					"type":       map[string]any{"displayName": "Build"},
				},
			}},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func adoPRSelectEnv(t *testing.T, repo providers.RepositoryRef, server *httptest.Server) {
	t.Helper()
	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token", func(p *providers.ADOProvider) {
			p.BaseURL = server.URL
		}), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_RUN_ID", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
}

// TestPRSelectDispatchesADOAndSelectsPolicyGreenPR is the end-to-end acceptance
// for pr-select's ADO branch: routed to an ADO repo, it never resolves a
// github:pr:write token, lists the active PR, derives CheckState from the
// approved Build policy, and selects it. headPrefixes is set to the ADO
// run-branch namespace so the goobers-authored PR is recognized as own — the
// config the merge-wiring-plan §8 advisoryMode trap requires.
func TestPRSelectDispatchesADOAndSelectsPolicyGreenPR(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	server := adoPRSelectServer(t, "approved")
	adoPRSelectEnv(t, repo, server)
	t.Setenv("GOOBERS_INPUT_HEADPREFIXES", "goobers/tb-ado-implementation/")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #359") {
		t.Fatalf("stdout = %q, want PR #359 selected via ADO branch-policy check state", stdout)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selected-pr.json"))
	if err != nil {
		t.Fatalf("read selected-pr.json: %v", err)
	}
	var selected map[string]string
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatalf("unmarshal selected-pr.json: %v", err)
	}
	if selected["number"] != "359" {
		t.Fatalf("selected number = %q, want 359", selected["number"])
	}
	if selected["advisoryMode"] != "false" {
		t.Fatalf("advisoryMode = %q, want false (branch matches headPrefixes)", selected["advisoryMode"])
	}
}

// TestPRSelectADOAdvisoryModeMisfireWhenHeadPrefixMismatch pins the
// merge-wiring-plan §8 trap: on ADO daemonIdentityAuthorLogin is "" (no
// AuthenticatedLogin), so isOwnPullRequest falls to the branch-prefix heuristic.
// With the default headPrefixes (goobers/implementation/) the ADO run branch
// (goobers/tb-ado-implementation/…) is NOT recognized as own, so under the
// default authorScope=goobers the otherwise-green PR is excluded and pr-select
// reports no work. This is the exact misfire that silently blocks the merge; the
// fix is config-side (set headPrefixes to the ADO namespace, as the sibling test
// does), not in this stage.
func TestPRSelectADOAdvisoryModeMisfireWhenHeadPrefixMismatch(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	server := adoPRSelectServer(t, "approved")
	adoPRSelectEnv(t, repo, server)
	// No GOOBERS_INPUT_HEADPREFIXES: defaults to goobers/implementation/.

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no eligible") && !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no-work: default headPrefixes must not recognize the ADO run branch as own", stdout)
	}
}

func TestPRSelectADOEnforcesOptInAndAssigneePolicy(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	server := adoPRSelectServer(t, "approved")
	adoPRSelectEnv(t, repo, server)
	t.Setenv("GOOBERS_INPUT_HEADPREFIXES", "goobers/tb-ado-implementation/")
	t.Setenv("GOOBERS_INPUT_REQUIREOPTINLABEL", "goobers:merge-review")
	t.Setenv("GOOBERS_INPUT_RESPECTASSIGNEE", "true")
	t.Setenv("GOOBERS_INPUT_SELFIDENTITY", "goobers-bot")

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "missing required opt-in label") {
		t.Fatalf("stdout = %q, want ADO candidate rejected by opt-in policy", stdout)
	}
	if !strings.Contains(stdout, "no eligible PR") {
		t.Fatalf("stdout = %q, want no eligible ADO PR", stdout)
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json")); err == nil {
		if strings.Contains(string(data), `"number"`) {
			t.Fatalf("no-work result = %q, want no selected PR", data)
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("read claimed-item.json: %v", err)
	}
}
