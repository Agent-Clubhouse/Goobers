package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

type adoSelectionReadFixture struct {
	mu             sync.Mutex
	lists, details int
	policyIDs      []string
	prs            []map[string]any
}

func adoSelectionReadPR(number int) map[string]any {
	return map[string]any{
		"pullRequestId": number, "status": "active", "title": "Change", "description": "Fixes #5",
		"reviewers":     []map[string]any{{"uniqueName": "bot", "vote": 0}},
		"sourceRefName": "refs/heads/goobers/implementation/" + strconv.Itoa(number), "targetRefName": "refs/heads/main",
		"createdBy": map[string]string{"uniqueName": "bot"}, "labels": []map[string]string{{"name": "review-opt-in"}},
		"lastMergeSourceCommit": map[string]string{"commitId": "head-" + strconv.Itoa(number)},
		"lastMergeTargetCommit": map[string]string{"commitId": "base"},
		"repository":            map[string]any{"id": "repo-id", "name": "web", "project": map[string]string{"id": "project-id", "name": "project"}},
	}
}

func (f *adoSelectionReadFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	const prefix = "/acme/project/_apis/git/repositories/web/pullrequests"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == prefix:
			f.lists++
			_ = json.NewEncoder(w).Encode(map[string]any{"value": f.prs})
		case strings.HasPrefix(r.URL.Path, prefix+"/"):
			f.details++
			n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, prefix+"/"))
			if err != nil {
				t.Error(err)
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(adoSelectionReadPR(n))
		case r.URL.Path == "/acme/project/_apis/policy/evaluations":
			id := r.URL.Query().Get("artifactId")
			id = id[strings.LastIndex(id, "/")+1:]
			f.policyIDs = append(f.policyIDs, id)
			status := "approved"
			if id == "2" {
				status = "running"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"status": status, "configuration": map[string]any{"id": 1, "isEnabled": true, "isBlocking": true, "type": map[string]string{"displayName": "Build"}}}}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func adoSelectionSourceForTest(t *testing.T, fixture *adoSelectionReadFixture) branchPolicyPRSelectSource {
	t.Helper()
	server := fixture.server(t)
	p := providers.NewADOProvider("acme", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
	return branchPolicyPRSelectSource{provider: p, snapshots: p}
}

func TestADOSelectionSnapshotNarrowsReadsWithoutLosingMetadata(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		t.Run(strconv.FormatBool(targeted), func(t *testing.T) {
			fixture := &adoSelectionReadFixture{}
			for n := 1; n <= 10; n++ {
				fixture.prs = append(fixture.prs, adoSelectionReadPR(n))
			}
			fixture.prs[9]["mergeStatus"] = "conflicts"
			fixture.prs[2]["isDraft"] = true
			fixture.prs[3]["labels"] = []map[string]string{{"name": providers.LabelNeedsHuman}}
			fixture.prs[4]["sourceRefName"] = "refs/heads/human/feature"
			fixture.prs[5]["labels"] = []map[string]string{}
			fixture.prs[6]["labels"] = []map[string]string{{"name": mergeReadyLabel}}
			fixture.prs[7]["reviewers"] = []map[string]any{{"uniqueName": "another-reviewer", "vote": 0}}
			fixture.prs[8]["createdBy"] = map[string]string{"uniqueName": "other-author"}
			source := adoSelectionSourceForTest(t, fixture)
			req := prSelectSourceRequest{base: "main", headPrefixes: []string{"goobers/implementation/"}, authorScope: authorScopeGoobers,
				completeness: prSelectCompleteSnapshot, excludeLabels: []string{providers.LabelNeedsHuman, mergeReadyLabel},
				requiredOptInLabel: "review-opt-in", respectAssignee: true, selfIdentity: "bot", identityFilters: providers.ListPullRequestsRequest{Author: "bot"}}
			if targeted {
				req.triggerRef = "github-webhook:pull_request#1"
			}
			repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "project", Name: "web"}
			prs, open, err := source.pullRequests(context.Background(), repo, req)
			if err != nil {
				t.Fatal(err)
			}
			if len(open) != 10 || len(prs) != 8 {
				t.Fatalf("snapshot lost metadata: open=%d candidates=%d", len(open), len(prs))
			}
			byNumber := map[int]providers.PullRequestSummary{}
			for _, pr := range prs {
				byNumber[pr.Number] = pr
			}
			if byNumber[1].CheckState != providers.CheckStatePassing || byNumber[2].CheckState != providers.CheckStatePending {
				t.Fatalf("eligible check states lost: %+v", prs)
			}
			if byNumber[10].Mergeable == nil || *byNumber[10].Mergeable || byNumber[10].CheckState != "" {
				t.Fatalf("conflict lost or needlessly checked: %+v", byNumber[10])
			}
			if !byNumber[3].Draft || byNumber[3].State != "open" || byNumber[3].Body != "Fixes #5" {
				t.Fatalf("metadata projection incomplete: %+v", byNumber[3])
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.lists != 1 || fixture.details != 0 || strings.Join(fixture.policyIDs, ",") != "1,2" {
				t.Fatalf("reads: list=%d detail=%d policies=%v", fixture.lists, fixture.details, fixture.policyIDs)
			}
		})
	}
}

func TestADOSelectionPartialWebhookAvoidsUnrelatedListAndPolicies(t *testing.T) {
	fixture := &adoSelectionReadFixture{prs: []map[string]any{adoSelectionReadPR(1)}}
	source := adoSelectionSourceForTest(t, fixture)
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "project", Name: "web"}
	req := prSelectSourceRequest{base: "main", headPrefixes: []string{"goobers/implementation/"}, authorScope: authorScopeGoobers, triggerRef: "github-webhook:pull_request#13"}
	prs, open, err := source.pullRequests(t.Context(), repo, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].Number != 13 || len(open) != 0 {
		t.Fatalf("unexpected targeted snapshot: %v %v", prs, open)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.lists != 0 || fixture.details != 1 || strings.Join(fixture.policyIDs, ",") != "13" {
		t.Fatalf("reads: list=%d detail=%d policies=%v", fixture.lists, fixture.details, fixture.policyIDs)
	}
}
