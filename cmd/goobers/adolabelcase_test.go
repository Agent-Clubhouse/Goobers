package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/providers"
)

// TestListBacklogScanWindowADOFoldsPredicateLabels drives the selection path
// cmd/goobers really uses on ADO: the label predicate is applied here, not in
// the provider, so the scan hands the provider the predicate's labels as
// CompareLabels. An excluded label a human first wrote as Needs-Design must
// still exclude the item rather than fail open.
func TestListBacklogScanWindowADOFoldsPredicateLabels(t *testing.T) {
	tags := map[int]string{1: "Goobers:Approved; Needs-Design", 2: "goobers:approved; Team-A"}
	server := newADOLabelCaseScanServer(t, tags)
	provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
	filter, err := labelpredicate.Compile(`"team-a" in labels`, nil, []string{"needs-design"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	items, _, err := listBacklogScanWindow(
		context.Background(), provider, providers.RepositoryRef{Name: "repo", Project: "project"},
		[]string{providers.LabelApproved}, filter.Labels(), "", nil, backlogScanCeiling, backlogScanCursor{}, false,
	)
	if err != nil {
		t.Fatalf("listBacklogScanWindow: %v", err)
	}
	var selected []string
	for _, item := range items {
		matched, err := filter.Matches(item.Labels)
		if err != nil {
			t.Fatalf("Matches: %v", err)
		}
		if matched && item.HasLabel(providers.LabelApproved) {
			selected = append(selected, item.ID)
		}
	}
	if !slices.Equal(selected, []string{"2"}) {
		t.Fatalf("selected = %v, want only item 2 (item 1 carries the excluded label)", selected)
	}
}

// newADOLabelCaseScanServer serves a WIQL query and batch read over tags, one
// open work item per key.
func newADOLabelCaseScanServer(t *testing.T, tags map[int]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, _ *http.Request) {
		refs := make([]map[string]int, 0, len(tags))
		for id := 1; id <= len(tags); id++ {
			refs = append(refs, map[string]int{"id": id})
		}
		writeADOJSON(t, w, map[string]interface{}{"workItems": refs})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []int `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode workitemsbatch request body: %v", err)
		}
		values := make([]map[string]interface{}, 0, len(body.IDs))
		for _, id := range body.IDs {
			values = append(values, map[string]interface{}{
				"id": id,
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.Title":        "item " + strconv.Itoa(id),
					"System.State":        "Active",
					"System.Tags":         tags[id],
				},
			})
		}
		writeADOJSON(t, w, map[string]interface{}{"count": len(values), "value": values})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, _ *http.Request) {
		writeADOJSON(t, w, map[string]interface{}{"value": []map[string]string{
			{"name": "Active", "category": "InProgress"},
		}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestContinuationEligibilityFoldsADOLabelCase pins that a continuation's
// re-check of a source item read with GetWorkItem compares labels ignoring
// case on ADO: an exclude tag a human first wrote as Needs-Design stops the
// continuation, and a required tag in another case still satisfies it. On
// GitHub the comparison stays exact.
func TestContinuationEligibilityFoldsADOLabelCase(t *testing.T) {
	filter, err := labelpredicate.Compile("", []string{"team-a"}, []string{"needs-design"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	policy := &continuationEligibilityPolicy{
		requireLabels: []string{"team-a"}, excludeLabels: []string{"needs-design"}, labelFilter: filter,
	}
	excluded := providers.WorkItem{ID: "7", Labels: []string{"Team-A", "Needs-Design"}}
	err = validateContinuationEligibility(excluded, providers.ProviderADO, "7", policy)
	if err == nil || !strings.Contains(err.Error(), `excluded label "needs-design"`) {
		t.Fatalf("ADO exclude in other case: err = %v, want the continuation stopped by the excluded label", err)
	}
	eligible := providers.WorkItem{ID: "8", Labels: []string{"TEAM-A"}}
	if err := validateContinuationEligibility(eligible, providers.ProviderADO, "8", policy); err != nil {
		t.Fatalf("ADO require in other case: err = %v, want eligible", err)
	}
	if err := validateContinuationEligibility(eligible, providers.ProviderGitHub, "8", policy); err == nil {
		t.Fatal("GitHub require in other case: want the exact comparison unchanged (not eligible)")
	}
}

// TestParentEligibleForDecompositionFoldsADOTrustLabel pins that a custom
// trustLabel matches an ADO tag first written in another case, and that
// GitHub keeps its exact comparison.
func TestParentEligibleForDecompositionFoldsADOTrustLabel(t *testing.T) {
	item := providers.WorkItem{ID: "7", State: "open", Labels: []string{"Example:Trusted"}}
	if !parentEligibleForDecomposition(item, providers.ProviderADO, "example:trusted") {
		t.Fatal("ADO parent with trust tag in other case: want eligible")
	}
	if parentEligibleForDecomposition(item, providers.ProviderGitHub, "example:trusted") {
		t.Fatal("GitHub parent with trust label in other case: want the exact comparison unchanged")
	}
	if !slices.Equal(item.Labels, []string{"Example:Trusted"}) {
		t.Fatalf("caller's labels mutated to %v", item.Labels)
	}
}
