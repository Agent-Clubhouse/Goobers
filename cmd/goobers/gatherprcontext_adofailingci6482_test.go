package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// probePlanForTest is a plan whose claim filter keeps everything and whose
// window is the fresh-cursor window, recording every probe.
func probePlanForTest(probed *[]int, failing ...int) adoCIProbePlan {
	fail := map[int]bool{}
	for _, n := range failing {
		fail[n] = true
	}
	return adoCIProbePlan{
		available: func(prs []providers.PullRequestSummary) ([]providers.PullRequestSummary, error) { return prs, nil },
		window: func(probeable []int) ([]int, error) {
			window, _ := adoCIProbeWindow(probeable, 0, adoRemediationCIProbeLimit)
			return window, nil
		},
		probe: func(_ context.Context, pr *providers.PullRequestSummary) error {
			*probed = append(*probed, pr.Number)
			if fail[pr.Number] {
				pr.CheckState = providers.CheckStateFailing
			}
			return nil
		},
	}
}

// TestADORemediationCandidatesDiscoverFailingCI pins the Azure DevOps
// eligibility pass (#6482): only PRs whose tier a CI read can change are
// probed, and a probe that finds failing CI promotes the PR into the strong
// tier.
func TestADORemediationCandidatesDiscoverFailingCI(t *testing.T) {
	prs := []providers.PullRequestSummary{
		{Number: 40, Head: "b/40"},
		{Number: 10, Head: "b/10", Labels: []string{needsRemediationLabel}},
		{Number: 20, Head: "b/20", Labels: []string{"Goobers:Merge-Escalated"}},
		{Number: 30, Head: "b/30"},
		{Number: 35, Head: "b/held"},
		{Number: 36, Head: "b/36", Labels: []string{providers.LabelNeedsHuman}},
	}
	var probed []int
	eligible, err := adoRemediationCandidates(context.Background(), prs, map[string]bool{"b/held": true}, probePlanForTest(&probed, 40))
	if err != nil {
		t.Fatalf("adoRemediationCandidates: %v", err)
	}
	if fmt.Sprint(probed) != "[40 30]" && fmt.Sprint(probed) != "[30 40]" {
		t.Fatalf("probed = %v, want only the unlabelled, unparked, unheld PRs #30 and #40", probed)
	}
	candidates, _ := strongRemediationCandidates(eligible)
	if fmt.Sprint(prNumbers(candidates)) != "[10 40]" {
		t.Fatalf("strong candidates = %v, want the labelled PR and the discovered failing-CI PR", prNumbers(candidates))
	}

	t.Run("PRs another run holds are not probed", func(t *testing.T) {
		var probed []int
		plan := probePlanForTest(&probed)
		plan.available = func(prs []providers.PullRequestSummary) ([]providers.PullRequestSummary, error) {
			var out []providers.PullRequestSummary
			for _, pr := range prs {
				if pr.Number != 30 {
					out = append(out, pr)
				}
			}
			return out, nil
		}
		if _, err := adoRemediationCandidates(context.Background(), prs, map[string]bool{"b/held": true}, plan); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(probed) != "[40]" {
			t.Fatalf("probed = %v, want #30 (claimed elsewhere) skipped", probed)
		}
	})

	t.Run("an unreadable CI state fails closed", func(t *testing.T) {
		plan := probePlanForTest(new([]int))
		plan.probe = func(context.Context, *providers.PullRequestSummary) error { return errors.New("401") }
		if _, err := adoRemediationCandidates(context.Background(), []providers.PullRequestSummary{{Number: 7, Head: "b/7"}}, nil, plan); err == nil {
			t.Fatal("a failed policy read was treated as passing CI")
		}
	})
}

// TestADORemediationCandidatesTargetedRunReadsOnlyItsTarget pins the review
// finding on targeted runs: `--pr N` reads N's CI alone, however many older
// probeable PRs there are (so N outside the bounded window is never refused as
// "CI is not failing" unread), and an unrelated PR whose read would fail
// cannot fail the operator's run.
func TestADORemediationCandidatesTargetedRunReadsOnlyItsTarget(t *testing.T) {
	prs := make([]providers.PullRequestSummary, 0, adoRemediationCIProbeLimit+10)
	for n := 1; n <= adoRemediationCIProbeLimit+10; n++ {
		prs = append(prs, providers.PullRequestSummary{Number: n, Head: "b/" + strconv.Itoa(n)})
	}
	target := adoRemediationCIProbeLimit + 7
	var probed []int
	plan := probePlanForTest(&probed, target)
	plan.target = remediationTarget{number: target, targeted: true}
	plan.window = func([]int) ([]int, error) {
		t.Fatal("a targeted run advanced the shared probe window")
		return nil, nil
	}
	inner := plan.probe
	plan.probe = func(ctx context.Context, pr *providers.PullRequestSummary) error {
		if pr.Number != target {
			return errors.New("unrelated PR's read failed")
		}
		return inner(ctx, pr)
	}
	eligible, err := adoRemediationCandidates(context.Background(), prs, nil, plan)
	if err != nil {
		t.Fatalf("adoRemediationCandidates: %v", err)
	}
	if fmt.Sprint(probed) != fmt.Sprintf("[%d]", target) {
		t.Fatalf("probed = %v, want only the target #%d", probed, target)
	}
	candidates, _ := strongRemediationCandidates(eligible)
	if fmt.Sprint(prNumbers(candidates)) != fmt.Sprintf("[%d]", target) {
		t.Fatalf("strong candidates = %v, want the target with its failing CI", prNumbers(candidates))
	}
}

// TestADOCIProbeWindowRotatesOverTheWholeSet pins the review finding on the
// bound: the window rotates, so a set larger than the bound is read in full
// over successive cycles instead of the same oldest PRs every time.
func TestADOCIProbeWindowRotatesOverTheWholeSet(t *testing.T) {
	probeable := make([]int, 0, 60)
	for n := 1; n <= 60; n++ {
		probeable = append(probeable, n*2)
	}
	seen := map[int]int{}
	cursor := 0
	for cycle := 1; cycle <= 3; cycle++ {
		window, next := adoCIProbeWindow(probeable, cursor, 25)
		if len(window) != 25 {
			t.Fatalf("cycle %d window has %d PRs, want the bound 25", cycle, len(window))
		}
		if cycle == 1 && (window[0] != 2 || window[24] != 50) {
			t.Fatalf("first window = %v, want the oldest 25 from a fresh cursor", window)
		}
		for _, n := range window {
			seen[n]++
		}
		cursor = next
	}
	if len(seen) != len(probeable) {
		t.Fatalf("three cycles probed %d distinct PRs, want all %d", len(seen), len(probeable))
	}
	if window, next := adoCIProbeWindow(probeable[:10], 999, 25); len(window) != 10 || next != 0 {
		t.Fatalf("small set: window %v next %d, want the whole set and a reset cursor", window, next)
	}
}

// TestAdvanceADOCIProbeWindowPersistsTheCursor drives the persisted cursor
// through the instance's scheduler state: consecutive cycles continue where
// the last one stopped, per repository.
func TestAdvanceADOCIProbeWindowPersistsTheCursor(t *testing.T) {
	l := layoutFor(initDemo(t))
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "proj", Name: "web"}
	probeable := make([]int, 0, 30)
	for n := 1; n <= 30; n++ {
		probeable = append(probeable, n)
	}
	first, err := advanceADOCIProbeWindow(l, repo, probeable)
	if err != nil {
		t.Fatalf("first window: %v", err)
	}
	second, err := advanceADOCIProbeWindow(l, repo, probeable)
	if err != nil {
		t.Fatalf("second window: %v", err)
	}
	if first[0] != 1 || first[len(first)-1] != 25 || second[0] != 26 {
		t.Fatalf("windows = %v then %v, want 1..25 then continuing at 26", first, second)
	}
	other := repo
	other.Name = "api"
	fresh, err := advanceADOCIProbeWindow(l, other, probeable)
	if err != nil {
		t.Fatalf("other repository: %v", err)
	}
	if fresh[0] != 1 {
		t.Fatalf("another repository's window = %v, want its own fresh cursor", fresh)
	}
}

// TestGatherPRContextADOSelectsUnlabelledFailingBuild is #6482's headline Azure
// DevOps scenario end to end: an open goober PR carrying no label, whose
// blocking build-validation policy a human's push turned red, is selected by
// a scheduled pr-remediation run — without waiting for merge-review to demote
// it — and its brief routes to CI remediation. Its policy evaluations are read
// once per cycle, not again for the selected PR.
func TestGatherPRContextADOSelectsUnlabelledFailingBuild(t *testing.T) {
	const (
		prBranch = "goobers/impl/run-ado-362"
		prNumber = 359
		login    = "merge-review-bot"
	)
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)

	prBase := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests"
	mux := http.NewServeMux()
	mux.HandleFunc(prBase, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{
			map[string]interface{}{
				"pullRequestId": prNumber, "status": "active",
				"sourceRefName": "refs/heads/" + prBranch, "targetRefName": "refs/heads/main",
				"createdBy":             map[string]string{"displayName": "goober", "uniqueName": "goober@example.com"},
				"lastMergeSourceCommit": map[string]string{"commitId": headSHA},
				"lastMergeTargetCommit": map[string]string{"commitId": baseSHA},
				"_links":                map[string]interface{}{"web": map[string]string{"href": "https://dev.azure.test/pr/359"}},
			},
		}})
	})
	mux.HandleFunc(prBase+"/"+strconv.Itoa(prNumber)+"/threads", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})
	mux.HandleFunc(prBase+"/"+strconv.Itoa(prNumber)+"/labels", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})
	evaluations := 0
	countingMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+repo.Owner+"/"+repo.Project+"/_apis/policy/evaluations" {
			evaluations++
		}
		mux.ServeHTTP(w, r)
	})
	handleADOSelectedPRRejectedBuild(t, mux, repo, prNumber, headSHA, baseSHA)
	mux.HandleFunc("/"+repo.Owner+"/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{
			"authenticatedUser": map[string]string{"id": "merge-review-bot-guid", "providerDisplayName": login},
		})
	})
	server := httptest.NewServer(countingMux)
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
		RepoURL: origin, RunID: "run-ado-6482", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-ado-6482",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })
	t.Setenv("GOOBERS_RUN_ID", "run-ado-6482")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	setDeliveredADOStageCredentials(t)
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", root)
	if code != 0 {
		t.Fatalf("gather-pr-context (ADO): code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(wt.Path, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read brief: %v (stdout %q)", err, stdout)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		t.Fatalf("decode brief %s: %v", data, err)
	}
	if brief.SelectedNumber != strconv.Itoa(prNumber) || brief.HasFailingCI != "true" {
		t.Fatalf("brief selected #%s hasFailingCI=%s, want #%d with failing CI: an unlabelled red build must be remediated", brief.SelectedNumber, brief.HasFailingCI, prNumber)
	}
	if evaluations != 1 {
		t.Fatalf("policy evaluations read %d times, want once: the selection probe already established the selected PR's CI state", evaluations)
	}
}
