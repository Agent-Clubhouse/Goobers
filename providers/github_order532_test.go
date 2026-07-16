package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListWorkItemsOldestFirstSetsAscendingSort pins the query contract behind
// ListWorkItemsRequest.OldestFirst (#532): GitHub's issues list defaults to
// newest-first, so a FIFO consumer must ask for sort=created&direction=asc
// explicitly — otherwise a Limit-truncated fetch permanently starves the
// oldest items. The default (OldestFirst unset) must NOT send the params:
// existing callers keep GitHub's default ordering unchanged.
func TestListWorkItemsOldestFirstSetsAscendingSort(t *testing.T) {
	var gotSort, gotDirection []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotSort = append(gotSort, q.Get("sort"))
		gotDirection = append(gotDirection, q.Get("direction"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}

	// OldestFirst on the paginated (no explicit Page) path.
	if _, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{Repository: repo, OldestFirst: true, Limit: 5}); err != nil {
		t.Fatalf("ListWorkItems (OldestFirst): %v", err)
	}
	// OldestFirst on the caller-driven single-page path.
	if _, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{Repository: repo, OldestFirst: true, Limit: 5, Page: 2}); err != nil {
		t.Fatalf("ListWorkItems (OldestFirst, Page): %v", err)
	}
	// Default: no ordering params at all.
	if _, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{Repository: repo, Limit: 5}); err != nil {
		t.Fatalf("ListWorkItems (default): %v", err)
	}

	if len(gotSort) != 3 {
		t.Fatalf("expected 3 requests, saw %d", len(gotSort))
	}
	for i := 0; i < 2; i++ {
		if gotSort[i] != "created" || gotDirection[i] != "asc" {
			t.Fatalf("request %d: sort=%q direction=%q, want created/asc", i, gotSort[i], gotDirection[i])
		}
	}
	if gotSort[2] != "" || gotDirection[2] != "" {
		t.Fatalf("default request sent sort=%q direction=%q, want neither", gotSort[2], gotDirection[2])
	}
}

// TestADOListWorkItemsOldestFirstSetsOrderBy is the ADO-provider counterpart
// to TestListWorkItemsOldestFirstSetsAscendingSort (#532, QA-3 coverage gap):
// WIQL has no implicit order, so OldestFirst must append an explicit
// ORDER BY on System.Id (ascending with creation order) — the same
// truncation-starvation hazard as GitHub's newest-first default, just via a
// missing ORDER BY instead of an explicit descending one. The default
// (OldestFirst unset) path must NOT append it, leaving existing query
// behavior unchanged.
func TestADOListWorkItemsOldestFirstSetsOrderBy(t *testing.T) {
	var gotQueries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode WIQL request body: %v", err)
		}
		gotQueries = append(gotQueries, body.Query)
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}

	if _, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{Repository: repo, OldestFirst: true}); err != nil {
		t.Fatalf("ListWorkItems (OldestFirst): %v", err)
	}
	if _, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{Repository: repo}); err != nil {
		t.Fatalf("ListWorkItems (default): %v", err)
	}

	if len(gotQueries) != 2 {
		t.Fatalf("expected 2 WIQL requests, saw %d", len(gotQueries))
	}
	if !strings.Contains(gotQueries[0], "ORDER BY [System.Id] ASC") {
		t.Fatalf("OldestFirst query = %q, want it to contain ORDER BY [System.Id] ASC", gotQueries[0])
	}
	if strings.Contains(gotQueries[1], "ORDER BY") {
		t.Fatalf("default query = %q, want no ORDER BY clause", gotQueries[1])
	}
}
