package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestADOWorkItemStateClause pins the WIQL translation of the provider-neutral
// State values. "open"/"closed" have no literal ADO equivalent and must expand
// to the terminal-state set (NOT IN / IN); any other value stays an exact match
// so a subscription passing a concrete ADO state (e.g. "New") still works.
func TestADOWorkItemStateClause(t *testing.T) {
	cases := []struct {
		state    string
		contains string
		absent   string
	}{
		{"open", "NOT IN (", "= 'open'"},
		{"Open", "NOT IN (", ""},
		{"closed", "] IN (", "NOT IN"},
		{"New", "= 'New'", "IN ("},
	}
	for _, tc := range cases {
		got := adoWorkItemStateClause(tc.state)
		if !strings.Contains(got, tc.contains) {
			t.Fatalf("adoWorkItemStateClause(%q) = %q, want it to contain %q", tc.state, got, tc.contains)
		}
		if tc.absent != "" && strings.Contains(got, tc.absent) {
			t.Fatalf("adoWorkItemStateClause(%q) = %q, want it to NOT contain %q", tc.state, got, tc.absent)
		}
	}
}

// TestADOUnifiedState pins the raw-ADO-state → open/closed projection the CLI
// backlog logic compares against. Terminal states collapse to "closed",
// everything else to "open", and an empty state stays empty (so callers can
// tell "provider didn't report state" apart from "open").
func TestADOUnifiedState(t *testing.T) {
	cases := map[string]string{
		"New":      "open",
		"Active":   "open",
		"Doing":    "open",
		"Closed":   "closed",
		"Done":     "closed",
		"Removed":  "closed",
		"resolved": "closed",
		"":         "",
	}
	for raw, want := range cases {
		if got := adoUnifiedState(raw); got != want {
			t.Fatalf("adoUnifiedState(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestADOMergeLabels pins the add/remove label reconciliation UpdateWorkItem
// relies on: removed labels drop, added labels append, and the result is
// de-duplicated with order preserved.
func TestADOMergeLabels(t *testing.T) {
	got := mergeLabels(
		[]string{"route/backend", "goobers/status:claimed", "keep"},
		[]string{"goobers/status:in-progress", "keep"},
		[]string{"goobers/status:claimed"},
	)
	want := []string{"route/backend", "keep", "goobers/status:in-progress"}
	if len(got) != len(want) {
		t.Fatalf("mergeLabels = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergeLabels[%d] = %q, want %q (full %#v)", i, got[i], want[i], got)
		}
	}
}

// TestADOListWorkItemsOpenStateFiltersAndNormalizes exercises the end-to-end
// backlog read: State:"open" must send a NOT-IN-terminal-set WIQL predicate
// (so it actually matches New/Active rather than the literal state 'open'), and
// the returned WorkItem.State must be normalized to the unified "open" while
// Status still derives from the raw ADO state.
func TestADOListWorkItemsOpenStateFiltersAndNormalizes(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode WIQL: %v", err)
		}
		gotQuery = body.Query
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 7}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"id": 7, "rev": 1, "url": "item-url",
			"fields": map[string]interface{}{
				"System.WorkItemType": "Product Backlog Item",
				"System.Title":        "Migrate WorkerContracts",
				"System.State":        "Active",
				"System.Tags":         "example-label",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{Repository: repo, State: "open"})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if !strings.Contains(gotQuery, "NOT IN (") {
		t.Fatalf("query = %q, want a NOT IN terminal-state clause", gotQuery)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].State != "open" {
		t.Fatalf("item State = %q, want normalized \"open\"", items[0].State)
	}
}

// TestADOListWorkItemsFiltersByTagsInWIQL pins the server-side tag filter: the
// backlog label(s) must land in the WIQL as [System.Tags] CONTAINS predicates.
// Without them a large project returns every open item and ADO 400s past its
// 20000-row cap before any client-side label filtering runs.
func TestADOListWorkItemsFiltersByTagsInWIQL(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode WIQL: %v", err)
		}
		gotQuery = body.Query
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	if _, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{Repository: repo, State: "open", Labels: []string{"example-label"}}); err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if !strings.Contains(gotQuery, "[System.Tags] CONTAINS 'example-label'") {
		t.Fatalf("query = %q, want it to filter by [System.Tags] CONTAINS 'example-label'", gotQuery)
	}
}

// TestADOUpdateWorkItemRemovesTagWithReplace pins the fix for ADO's
// union-only System.Tags "add": removing a label (e.g. releasing the
// goobers/status:claimed marker at close-out) must emit a "replace" carrying
// the full remaining set, or the tag would never actually come off.
func TestADOUpdateWorkItemRemovesTagWithReplace(t *testing.T) {
	var patch []adoPatchOperation
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 42, "rev": 1, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Product Backlog Item",
					"System.Title":        "Fix",
					"System.State":        "Active",
					"System.Tags":         "goobers/status:claimed; example-label",
				},
			})
		case http.MethodPatch:
			decodeJSON(t, r, &patch)
			writeJSON(t, w, map[string]interface{}{
				"id": 42, "rev": 2, "url": "item-url",
				"fields": map[string]interface{}{"System.Tags": "example-label"},
			})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	if _, err := provider.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository:   repo,
		ID:           "42",
		RemoveLabels: []string{"goobers/status:claimed"},
	}); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	var tagsOp *adoPatchOperation
	for i := range patch {
		if patch[i].Path == "/fields/System.Tags" {
			tagsOp = &patch[i]
		}
	}
	if tagsOp == nil {
		t.Fatalf("no System.Tags op in patch %#v", patch)
	}
	if tagsOp.Op != "replace" {
		t.Fatalf("System.Tags op = %q, want \"replace\" (add unions and cannot remove)", tagsOp.Op)
	}
	if got, ok := tagsOp.Value.(string); !ok || got != "example-label" {
		t.Fatalf("System.Tags value = %#v, want \"example-label\" (claimed removed)", tagsOp.Value)
	}
}

// TestADOFindPullRequestByBranch pins the exact source-branch match the
// idempotent OpenPullRequest and issue-close-out linking rely on: a prefix
// collision ("run-1" vs "run-10") must not resolve the wrong PR.
func TestADOFindPullRequestByBranch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
			{"pullRequestId": 10, "url": "pr-10", "sourceRefName": "refs/heads/run-10", "targetRefName": "refs/heads/main"},
			{"pullRequestId": 1, "url": "pr-1", "sourceRefName": "refs/heads/run-1", "targetRefName": "refs/heads/main"},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}

	pr, found, err := provider.FindPullRequestByBranch(context.Background(), repo, "run-1", "main")
	if err != nil {
		t.Fatalf("FindPullRequestByBranch: %v", err)
	}
	if !found || pr.Number != 1 {
		t.Fatalf("FindPullRequestByBranch(run-1) = %#v found=%v, want PR 1", pr, found)
	}

	if _, found, err := provider.FindPullRequestByBranch(context.Background(), repo, "run-999", "main"); err != nil || found {
		t.Fatalf("FindPullRequestByBranch(run-999) found=%v err=%v, want not found", found, err)
	}
}
