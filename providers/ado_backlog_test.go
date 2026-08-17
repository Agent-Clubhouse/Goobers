package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestADOMergeLabels pins the add/remove label reconciliation UpdateWorkItem
// relies on: removed labels drop, added labels append, and the result is
// de-duplicated with order preserved.
func TestADOMergeLabels(t *testing.T) {
	got := applyLabelSet(
		[]string{"route/backend", "goobers/status:claimed", "keep"},
		[]string{"goobers/status:in-progress", "keep"},
		[]string{"goobers/status:claimed"},
	)
	want := []string{"route/backend", "keep", "goobers/status:in-progress"}
	if len(got) != len(want) {
		t.Fatalf("applyLabelSet = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applyLabelSet[%d] = %q, want %q (full %#v)", i, got[i], want[i], got)
		}
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

func TestADOClaimFailsWhenWrittenBreadcrumbIsNotVisible(t *testing.T) {
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{"comments": []interface{}{}})
		case http.MethodPost:
			writeJSON(t, w, map[string]interface{}{"commentId": 1, "text": "accepted but not persisted"})
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"id": 42, "rev": 1,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.State":        "New",
				"System.Title":        "Claim candidate",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.ClaimWorkItem(context.Background(), ClaimWorkItemRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		ID:         "42",
		RunID:      "run-42",
	})
	if err == nil || !strings.Contains(err.Error(), "not visible after write") {
		t.Fatalf("ClaimWorkItem error = %v, want missing-breadcrumb failure", err)
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

func TestADODecompositionMarkerAndCommentMutations(t *testing.T) {
	const marker = "<!-- goobers-action:v1 key=child -->"
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != strconv.Itoa(adoWIQLPageSize) {
			t.Fatalf("$top = %q, want %d", got, adoWIQLPageSize)
		}
		var body struct {
			Query string `json:"query"`
		}
		decodeJSON(t, r, &body)
		if strings.Contains(body.Query, "Description") {
			t.Fatalf("query = %q, must not use full-text description search", body.Query)
		}
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}, {"id": 2}}})
	})
	for id, description := range map[int]string{1: "body\n" + marker, 2: "prefix " + marker} {
		mux.HandleFunc("/org/project/_apis/wit/workitems/"+strconv.Itoa(id), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]interface{}{
				"id": id, "rev": 1,
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.State":        "New",
					"System.Description":  description,
				},
			})
		})
	}
	mux.HandleFunc("/org/project/_apis/wit/workItems/7/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body map[string]string
		decodeJSON(t, r, &body)
		writeJSON(t, w, map[string]interface{}{"commentId": 9, "text": body["text"]})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	items, err := provider.FindWorkItemsByMarker(context.Background(), repo, marker)
	if err != nil {
		t.Fatalf("FindWorkItemsByMarker: %v", err)
	}
	if len(items) != 1 || items[0].ID != "1" {
		t.Fatalf("items = %#v, want exact marker match #1", items)
	}
	comment, err := provider.CreateWorkItemComment(context.Background(), repo, "7", "prepared")
	if err != nil {
		t.Fatalf("CreateWorkItemComment: %v", err)
	}
	if comment.ID != "9" || comment.Body != "prepared" {
		t.Fatalf("comment = %#v", comment)
	}
}

func TestADOFindWorkItemsByMarkerPagesByID(t *testing.T) {
	const marker = "<!-- goobers-action:v1 key=second-page -->"
	var queries []string
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != "2" {
			t.Fatalf("$top = %q, want 2", got)
		}
		var body struct {
			Query string `json:"query"`
		}
		decodeJSON(t, r, &body)
		queries = append(queries, body.Query)
		switch {
		case strings.Contains(body.Query, "[System.Id] > 2"):
			writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 3}}})
		case strings.Contains(body.Query, "[System.Id] >"):
			t.Fatalf("unexpected cursor query %q", body.Query)
		default:
			writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}, {"id": 2}}})
		}
	})
	for id := 1; id <= 3; id++ {
		description := "body"
		if id == 3 {
			description += "\n" + marker
		}
		mux.HandleFunc("/org/project/_apis/wit/workitems/"+strconv.Itoa(id), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]interface{}{
				"id": id, "rev": 1,
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.State":        "New",
					"System.Description":  description,
				},
			})
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	items, err := provider.findWorkItemsByMarker(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		marker,
		2,
	)
	if err != nil {
		t.Fatalf("findWorkItemsByMarker: %v", err)
	}
	if len(items) != 1 || items[0].ID != "3" {
		t.Fatalf("items = %#v, want exact marker match #3", items)
	}
	if len(queries) != 2 {
		t.Fatalf("queries = %#v, want two pages", queries)
	}
	if !strings.HasSuffix(queries[0], "ORDER BY [System.Id] ASC") {
		t.Fatalf("first query = %q, want deterministic ID order", queries[0])
	}
	if !strings.HasSuffix(queries[1], "AND [System.Id] > 2 ORDER BY [System.Id] ASC") {
		t.Fatalf("second query = %q, want ID cursor after first page", queries[1])
	}
}
