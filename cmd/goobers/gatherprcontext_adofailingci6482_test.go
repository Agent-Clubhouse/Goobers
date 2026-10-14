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

// TestADORemediationCandidatesDiscoverFailingCI pins the Azure DevOps
// eligibility pass (#6482): only PRs whose tier a CI read can change are
// probed, oldest first and at most adoRemediationCIProbeLimit per cycle, and a
// probe that finds failing CI promotes the PR into the strong tier.
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
	probe := func(_ context.Context, pr *providers.PullRequestSummary) error {
		probed = append(probed, pr.Number)
		if pr.Number == 40 {
			pr.CheckState = providers.CheckStateFailing
		}
		return nil
	}

	eligible, err := adoRemediationCandidates(context.Background(), prs, map[string]bool{"b/held": true}, probe)
	if err != nil {
		t.Fatalf("adoRemediationCandidates: %v", err)
	}
	if fmt.Sprint(probed) != "[30 40]" {
		t.Fatalf("probed = %v, want only the unlabelled, unparked, unheld PRs, oldest first", probed)
	}
	candidates, _ := strongRemediationCandidates(eligible)
	if fmt.Sprint(prNumbers(candidates)) != "[10 40]" {
		t.Fatalf("strong candidates = %v, want the labelled PR and the discovered failing-CI PR", prNumbers(candidates))
	}

	t.Run("probes are bounded per cycle", func(t *testing.T) {
		many := make([]providers.PullRequestSummary, 0, adoRemediationCIProbeLimit+5)
		for n := adoRemediationCIProbeLimit + 5; n > 0; n-- {
			many = append(many, providers.PullRequestSummary{Number: n, Head: "b/" + strconv.Itoa(n)})
		}
		var count, highest int
		_, err := adoRemediationCandidates(context.Background(), many, nil, func(_ context.Context, pr *providers.PullRequestSummary) error {
			count++
			highest = max(highest, pr.Number)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if count != adoRemediationCIProbeLimit || highest != adoRemediationCIProbeLimit {
			t.Fatalf("probed %d PRs up to #%d, want the oldest %d", count, highest, adoRemediationCIProbeLimit)
		}
	})

	t.Run("an unreadable CI state fails closed", func(t *testing.T) {
		_, err := adoRemediationCandidates(context.Background(), []providers.PullRequestSummary{{Number: 7, Head: "b/7"}}, nil,
			func(context.Context, *providers.PullRequestSummary) error { return errors.New("401") })
		if err == nil {
			t.Fatal("a failed policy read was treated as passing CI")
		}
	})
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
