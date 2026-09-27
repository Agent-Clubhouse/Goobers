package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestADOFindWorkItemsByMarkerHydratesOneChunkAtATime pins that the marker
// scan hydrates and filters one workitemsbatch chunk before fetching the
// next, rather than loading a whole WIQL page of full items at once. The
// request order is the witness: the first chunk's items are mapped (their
// type's state table read) before the second chunk is requested.
func TestADOFindWorkItemsByMarkerHydratesOneChunkAtATime(t *testing.T) {
	const marker = "<!-- goobers-action:v1 key=chunked -->"
	const total = adoWorkItemsBatchSize + 50
	var (
		mu       sync.Mutex
		requests []string
	)
	record := func(entry string) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, entry)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, r *http.Request) {
		record("states " + strings.Split(strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitemtypes/"), "/")[0])
		writeJSON(t, w, map[string]interface{}{"value": []map[string]string{{"name": "New", "category": "Proposed"}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, _ *http.Request) {
		refs := make([]map[string]int, 0, total)
		for id := 1; id <= total; id++ {
			refs = append(refs, map[string]int{"id": id})
		}
		writeJSON(t, w, map[string]interface{}{"workItems": refs})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitems/"))
		if err != nil {
			t.Errorf("work item path %q: %v", r.URL.Path, err)
			http.NotFound(w, r)
			return
		}
		itemType, description := "Issue", "body"
		if id > adoWorkItemsBatchSize {
			itemType = "Bug"
		}
		if id == 5 || id == adoWorkItemsBatchSize+30 {
			description += "\n" + marker
		}
		writeJSON(t, w, map[string]interface{}{"id": id, "rev": 1, "fields": map[string]interface{}{
			"System.WorkItemType": itemType, "System.State": "New", "System.Description": description,
		}})
	})
	batches := withADOTestWorkItemsBatch(t, mux)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, adoWorkItemsBatchPath) {
			record("batch")
		}
		batches.ServeHTTP(w, r)
	}))
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	items, err := provider.findWorkItemsByMarker(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, marker, adoWIQLPageSize)
	if err != nil {
		t.Fatalf("findWorkItemsByMarker: %v", err)
	}
	var ids []string
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if want := []string{"5", strconv.Itoa(adoWorkItemsBatchSize + 30)}; !slices.Equal(ids, want) {
		t.Fatalf("matches = %v, want %v", ids, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"batch", "states Issue", "batch", "states Bug"}; !slices.Equal(requests, want) {
		t.Fatalf("request order = %v, want %v (each chunk mapped before the next is fetched)", requests, want)
	}
}
