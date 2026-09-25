package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
)

// TestADOProviderListOpenPullRequestsPagesAndKeepsLabels pins the ADO read the
// readiness.maxOpenPRs throttle polls: every active PR across pages, head
// stripped of refs/heads/, labels included, with no base or head filter.
func TestADOProviderListOpenPullRequestsPagesAndKeepsLabels(t *testing.T) {
	var skips []string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		q := r.URL.Query()
		if got := q.Get("searchCriteria.status"); got != "active" {
			t.Errorf("searchCriteria.status = %q", got)
		}
		if got := q.Get("includeLabels"); got != "true" {
			t.Errorf("includeLabels = %q, want true", got)
		}
		if got := q.Get("searchCriteria.targetRefName"); got != "" {
			t.Errorf("searchCriteria.targetRefName = %q, want unset", got)
		}
		skip := q.Get("$skip")
		skips = append(skips, skip)
		var page []map[string]interface{}
		if skip == "0" {
			for i := 0; i < adoPullRequestPageSize; i++ {
				page = append(page, map[string]interface{}{
					"pullRequestId": i + 1,
					"sourceRefName": fmt.Sprintf("refs/heads/goobers/implementation/run-%d", i+1),
				})
			}
		} else {
			page = []map[string]interface{}{{
				"pullRequestId": 500,
				"sourceRefName": "refs/heads/feature/human",
				"labels":        []map[string]string{{"name": "Goobers:Merge-Escalated"}},
			}}
		}
		writeJSON(t, w, map[string]interface{}{"value": page})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	prs, err := provider.ListOpenPullRequests(context.Background(), RepositoryRef{Provider: ProviderADO, Owner: "org", Project: "project", Name: "repo"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if want := []string{"0", strconv.Itoa(adoPullRequestPageSize)}; !slices.Equal(skips, want) {
		t.Fatalf("$skip sequence = %v, want %v", skips, want)
	}
	if len(prs) != adoPullRequestPageSize+1 {
		t.Fatalf("len(prs) = %d, want %d", len(prs), adoPullRequestPageSize+1)
	}
	if prs[0].Head != "goobers/implementation/run-1" {
		t.Fatalf("first head = %q", prs[0].Head)
	}
	last := prs[len(prs)-1]
	if last.Head != "feature/human" || len(last.Labels) != 1 || last.Labels[0] != "Goobers:Merge-Escalated" {
		t.Fatalf("last summary = %#v", last)
	}
}
