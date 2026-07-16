package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
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
