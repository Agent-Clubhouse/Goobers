package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// withADOTestWorkItemsBatch answers POST .../_apis/wit/workitemsbatch from
// the fixture's own GET .../_apis/wit/workitems/{id} handlers, so a fixture
// written against per-item reads also serves batch hydration. An id whose
// GET answers 404 comes back as null, as errorPolicy=Omit returns it; any
// other non-200 answer fails the whole batch with that status.
func withADOTestWorkItemsBatch(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, ok := strings.CutSuffix(r.URL.Path, adoWorkItemsBatchPath)
		if !ok || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, ok := decodeADOTestBatchRequest(t, w, r)
		if !ok {
			return
		}
		values := make([]json.RawMessage, 0, len(body.IDs))
		for _, id := range body.IDs {
			target := prefix + "/_apis/wit/workitems/" + strconv.Itoa(id) + "?%24expand=Relations&api-version=7.1"
			itemReq := httptest.NewRequest(http.MethodGet, target, nil)
			itemReq.Header = r.Header.Clone()
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, itemReq)
			switch rec.Code {
			case http.StatusOK:
				values = append(values, json.RawMessage(rec.Body.Bytes()))
			case http.StatusNotFound:
				values = append(values, json.RawMessage("null"))
			default:
				http.Error(w, rec.Body.String(), rec.Code)
				return
			}
		}
		writeJSON(t, w, map[string]interface{}{"count": len(values), "value": values})
	})
}

// decodeADOTestBatchRequest decodes and checks a workitemsbatch request
// body against the shape ADO accepts.
func decodeADOTestBatchRequest(t *testing.T, w http.ResponseWriter, r *http.Request) (adoWorkItemsBatchRequest, bool) {
	t.Helper()
	var body adoWorkItemsBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode workitemsbatch body: %v", err)
		http.Error(w, "bad body", http.StatusBadRequest)
		return body, false
	}
	if len(body.IDs) == 0 || len(body.IDs) > adoWorkItemsBatchSize || body.Expand != "Relations" || body.ErrorPolicy != "Omit" {
		t.Errorf("workitemsbatch body = %+v, want 1..%d ids, $expand Relations, errorPolicy Omit", body, adoWorkItemsBatchSize)
		http.Error(w, "bad body", http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// adoBatchTestServer serves WIQL hits 1..hits and answers workitemsbatch
// directly, recording each batch's ids and counting per-item GETs.
type adoBatchTestServer struct {
	mu       sync.Mutex
	batches  [][]int
	itemGets int
	tags     func(id int) string
}

func newADOBatchTestServer(t *testing.T, hits int, tags func(id int) string) (*adoBatchTestServer, *httptest.Server) {
	t.Helper()
	fake := &adoBatchTestServer{tags: tags}
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		refs := make([]map[string]int, 0, hits)
		for id := 1; id <= hits; id++ {
			refs = append(refs, map[string]int{"id": id})
		}
		writeJSON(t, w, map[string]interface{}{"workItems": refs})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.itemGets++
		fake.mu.Unlock()
		http.Error(w, "per-item GET not expected", http.StatusTeapot)
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		body, ok := decodeADOTestBatchRequest(t, w, r)
		if !ok {
			return
		}
		fake.mu.Lock()
		fake.batches = append(fake.batches, append([]int(nil), body.IDs...))
		fake.mu.Unlock()
		// Answer in reverse so the provider cannot lean on response order.
		values := make([]interface{}, 0, len(body.IDs))
		for i := len(body.IDs) - 1; i >= 0; i-- {
			values = append(values, fake.item(body.IDs[i]))
		}
		writeJSON(t, w, map[string]interface{}{"count": len(values), "value": values})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return fake, server
}

func (f *adoBatchTestServer) item(id int) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "rev": 1,
		"fields": map[string]interface{}{
			"System.WorkItemType": "Issue",
			"System.Title":        "item " + strconv.Itoa(id),
			"System.State":        "New",
			"System.Tags":         f.tags(id),
		},
		"relations": []map[string]interface{}{},
	}
}

func (f *adoBatchTestServer) batchSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	sizes := make([]int, len(f.batches))
	for i, batch := range f.batches {
		sizes[i] = len(batch)
	}
	return sizes
}

// TestADOListWorkItemsHydratesThroughWorkItemsBatch pins ADO-N33: 250 WIQL
// hits hydrate in two workitemsbatch calls (200 + 50) with no per-item GET,
// and the result keeps WIQL order even though the batch answers out of order.
func TestADOListWorkItemsHydratesThroughWorkItemsBatch(t *testing.T) {
	fake, server := newADOBatchTestServer(t, 250, func(int) string { return "wanted" })
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if got := fake.batchSizes(); len(got) != 2 || got[0] != 200 || got[1] != 50 {
		t.Fatalf("batch sizes = %v, want [200 50]", got)
	}
	if fake.itemGets != 0 {
		t.Fatalf("per-item GETs = %d, want 0", fake.itemGets)
	}
	if len(items) != 250 {
		t.Fatalf("len(items) = %d, want 250", len(items))
	}
	for i, item := range items {
		if want := strconv.Itoa(i + 1); item.ID != want {
			t.Fatalf("items[%d].ID = %q, want %q (WIQL order)", i, item.ID, want)
		}
	}
}

// TestADOListWorkItemsBatchKeepsEarlyBreakAndPageInfo pins that chunked
// hydration keeps #2067's early break and PageInfo semantics: once Limit
// matches are in hand no further batch is fetched, CandidateCount counts
// only the hits inspected, and NextCursor resumes after the last one.
func TestADOListWorkItemsBatchKeepsEarlyBreakAndPageInfo(t *testing.T) {
	// Only every third item from 150 on carries the label, so the second
	// match is hit 153 and the scan stops inside the first chunk.
	tags := func(id int) string {
		if id >= 150 && id%3 == 0 {
			return "wanted"
		}
		return "other"
	}
	fake, server := newADOBatchTestServer(t, 450, tags)
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	pageInfo := &ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Labels:     []string{"wanted"},
		Limit:      2,
		PageInfo:   pageInfo,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 2 || items[0].ID != "150" || items[1].ID != "153" {
		t.Fatalf("items = %v, want 150 and 153", workItemIDs(items))
	}
	if got := fake.batchSizes(); len(got) != 1 || got[0] != 200 {
		t.Fatalf("batch sizes = %v, want one batch of 200 (early break)", got)
	}
	if pageInfo.CandidateCount != 153 || !pageInfo.HasNext || pageInfo.NextCursor != "153" {
		t.Fatalf("page info = %+v, want CandidateCount 153, HasNext, NextCursor 153", pageInfo)
	}
}

// TestADOListWorkItemsBatchSkipsOmittedItem pins errorPolicy=Omit: an item
// deleted between the WIQL query and hydration comes back null and is
// skipped instead of failing the listing.
func TestADOListWorkItemsBatchSkipsOmittedItem(t *testing.T) {
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}, {"id": 2}, {"id": 3}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"count": 3, "value": []interface{}{
			map[string]interface{}{"id": 3, "fields": map[string]interface{}{"System.WorkItemType": "Issue", "System.State": "New"}},
			nil,
			map[string]interface{}{"id": 1, "fields": map[string]interface{}{"System.WorkItemType": "Issue", "System.State": "New"}},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if got := workItemIDs(items); len(got) != 2 || got[0] != "1" || got[1] != "3" {
		t.Fatalf("items = %v, want [1 3]", got)
	}
}

// TestADOWorkItemsBatchRetriesTransientFailure pins that the batch POST,
// which only reads, is resent after a 5xx the way the per-item GET it
// replaces was.
func TestADOWorkItemsBatchRetriesTransientFailure(t *testing.T) {
	attempts := 0
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(t, w, map[string]interface{}{"count": 1, "value": []interface{}{
			map[string]interface{}{"id": 7, "fields": map[string]interface{}{"System.WorkItemType": "Issue", "System.State": "New"}},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) {
		p.BaseURL = server.URL
		p.sleep = func(context.Context, time.Duration) error { return nil }
	})

	items, err := provider.getWorkItemsBatch(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, []int{7})
	if err != nil {
		t.Fatalf("getWorkItemsBatch: %v", err)
	}
	if len(items) != 1 || items[0].ID != 7 || attempts != 2 {
		t.Fatalf("items = %+v after %d attempts, want item 7 after one retry", items, attempts)
	}
}

func TestADORetryableRequest(t *testing.T) {
	cases := []struct {
		method, endpoint string
		want             bool
	}{
		{http.MethodGet, "https://dev.azure.com/org/project/_apis/wit/workitems/1", true},
		{http.MethodPost, "https://dev.azure.com/org/project/_apis/wit/workitemsbatch?api-version=7.1", true},
		{http.MethodPost, "https://dev.azure.com/org/project/_apis/wit/wiql?api-version=7.1", false},
		{http.MethodPost, "https://dev.azure.com/org/project/_apis/wit/workitems/$Issue?api-version=7.1", false},
		{http.MethodPatch, "https://dev.azure.com/org/project/_apis/wit/workitemsbatch", false},
	}
	for _, tc := range cases {
		if got := adoRetryableRequest(tc.method, tc.endpoint); got != tc.want {
			t.Errorf("adoRetryableRequest(%s, %s) = %v, want %v", tc.method, tc.endpoint, got, tc.want)
		}
	}
}

func workItemIDs(items []WorkItem) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}
