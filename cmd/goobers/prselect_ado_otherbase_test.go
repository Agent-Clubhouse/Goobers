package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestADOSelectionFullWebhookOtherBaseKeepsChecks(t *testing.T) {
	pr := adoSelectionReadPR(1)
	pr["targetRefName"] = "refs/heads/release"
	const path = "/acme/project/_apis/git/repositories/web/pullrequests"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case path:
			prs := []map[string]any{}
			if r.URL.Query().Get("searchCriteria.targetRefName") == "refs/heads/release" {
				prs = append(prs, pr)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": prs})
		case path + "/1":
			_ = json.NewEncoder(w).Encode(pr)
		case "/acme/project/_apis/policy/evaluations":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"status": "approved", "configuration": map[string]any{"id": 1, "isEnabled": true, "isBlocking": true, "type": map[string]string{"displayName": "Build"}}}}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := providers.NewADOProvider("acme", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
	req := prSelectSourceRequest{base: "main", headPrefixes: []string{"goobers/implementation/"}, authorScope: authorScopeGoobers, triggerRef: "github-webhook:pull_request#1", completeness: prSelectCompleteSnapshot}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Project: "project", Name: "web"}
	for _, optimized := range []bool{false, true} {
		source := branchPolicyPRSelectSource{provider: p}
		if optimized {
			source.snapshots = p
		}
		prs, _, err := source.pullRequests(context.Background(), repo, req)
		if err != nil {
			t.Fatal(err)
		}
		if len(prs) != 1 || prs[0].CheckState != providers.CheckStatePassing {
			t.Errorf("optimized=%v lost target policy state: %+v", optimized, prs)
		}
	}
}
