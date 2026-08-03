package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestListBacklogScanWindowADOOversizedScanDoesNotViolateInvariant is
// #2067's missing ADO-backed regression test (flagged in review: the
// existing TestBacklogQueryPredicateDoesNotExpandScanCeiling only exercises
// a GitHub fake server, so nothing in CI exercised this failure mode for
// ADO before this test existed).
//
// listBacklogScanWindow's per-page invariant used to assert
// PageInfo.CandidateCount <= pageLimit — an assumption that predates #2067
// and is no longer valid: ADO's ListWorkItems now scans more raw candidates
// than pageLimit in one call whenever a post-WIQL-only filter is active
// (here, the hardcoded State: "open", which requires each candidate's
// process-specific state category to be read before it can be compared).
// GitHub's identical #2067 fix never tripped this because its own
// per-page ceiling (100) happens to equal backlogScanPageSize (100) — a
// coincidence, not a guarantee. With 200 open ADO work items and
// pageLimit=100, the first call's CandidateCount is 200 (ADO's WIQL $top
// went to candidateScanCeiling=250 because state normalization is
// unconditionally a post-fetch filter on ADO) — exactly the shape Dev-4
// reproduced against the pre-fix branch.
func TestListBacklogScanWindowADOOversizedScanDoesNotViolateInvariant(t *testing.T) {
	const totalItems = 200
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode WIQL request body: %v", err)
		}
		afterID := 0
		if idx := strings.Index(body.Query, "[System.Id] > "); idx != -1 {
			rest := body.Query[idx+len("[System.Id] > "):]
			end := strings.IndexAny(rest, " \n")
			if end == -1 {
				end = len(rest)
			}
			n, err := strconv.Atoi(rest[:end])
			if err != nil {
				t.Fatalf("parse cursor id from query %q: %v", body.Query, err)
			}
			afterID = n
		}
		items := make([]map[string]int, 0, totalItems-afterID)
		for id := afterID + 1; id <= totalItems; id++ {
			items = append(items, map[string]int{"id": id})
		}
		writeADOJSON(t, w, map[string]interface{}{"workItems": items})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitems/")
		numericID, err := strconv.Atoi(id)
		if err != nil {
			t.Fatalf("parse work item id %q: %v", id, err)
		}
		writeADOJSON(t, w, map[string]interface{}{
			"id": numericID,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Active",
				"System.Title":        "item " + id,
				"System.State":        "Active",
			},
		})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, _ *http.Request) {
		writeADOJSON(t, w, map[string]interface{}{"value": []map[string]string{
			{"name": "Active", "category": "InProgress"},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
	repo := providers.RepositoryRef{Name: "repo", Project: "project"}

	items, _, err := listBacklogScanWindow(
		context.Background(), provider, repo, nil, "", nil, backlogScanCeiling, backlogScanCursor{}, false,
	)
	if err != nil {
		t.Fatalf("listBacklogScanWindow: %v (this is exactly the invariant #2067's ADO fix must not trip)", err)
	}
	if len(items) != totalItems {
		t.Fatalf("len(items) = %d, want %d (every open item found across the multi-page scan)", len(items), totalItems)
	}
}

func writeADOJSON(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
