package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestADOListPullRequestsLimitAndPage(t *testing.T) {
	var tops, skips []string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		tops = append(tops, r.URL.Query().Get("$top"))
		skips = append(skips, r.URL.Query().Get("$skip"))
		top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		var values []map[string]interface{}
		for i := 0; i < top; i++ {
			values = append(values, map[string]interface{}{"pullRequestId": skip + i + 1})
		}
		writeJSON(t, w, map[string]interface{}{"value": values})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	prs, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository:     RepositoryRef{Name: "repo", Project: "project"},
		Limit:          3,
		Page:           2,
		SkipCheckState: true,
	})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(prs) != 3 || prs[0].Number != 4 || prs[2].Number != 6 {
		t.Fatalf("prs = %#v, want #4..#6", prs)
	}
	if len(tops) != 1 || tops[0] != "3" || skips[0] != "3" {
		t.Fatalf("top=%v skip=%v, want a single $top=3 $skip=3 request", tops, skips)
	}
	for _, pr := range prs {
		if pr.CheckState != "" {
			t.Fatalf("CheckState = %q, want empty when skipped", pr.CheckState)
		}
	}
}

func TestADOListPullRequestsResolvesCheckState(t *testing.T) {
	var evalPulls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
			{"pullRequestId": 7, "repository": map[string]interface{}{"project": map[string]string{"id": "proj-id", "name": "project"}}},
		}})
	})
	mux.HandleFunc("/org/project/_apis/policy/evaluations", func(w http.ResponseWriter, r *http.Request) {
		evalPulls = append(evalPulls, r.URL.Query().Get("artifactId"))
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	prs, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{Repository: RepositoryRef{Name: "repo", Project: "project"}})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(prs) != 1 || prs[0].CheckState != CheckStatePassing {
		t.Fatalf("prs = %#v, want one passing PR", prs)
	}
	if want := "vstfs:///CodeReview/CodeReviewId/proj-id/7"; len(evalPulls) != 1 || evalPulls[0] != want {
		t.Fatalf("evaluations artifacts = %v, want [%s]", evalPulls, want)
	}
}
