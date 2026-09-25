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

// adoTestStates registers a per-type work-item state table under
// /org/project/_apis/wit/workitemtypes/<type>/states, so a test can give
// different types different Completed/Resolved shapes.
func adoTestStates(t *testing.T, mux *http.ServeMux, byType map[string][]map[string]string) {
	t.Helper()
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		itemType := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitemtypes/"), "/states")
		states, ok := byType[itemType]
		if !ok {
			t.Fatalf("unexpected work item type in states request: %q", itemType)
		}
		writeJSON(t, w, map[string]interface{}{"value": states})
	})
}

// TestADOCloseWorkItemAlreadyCompletedIsNoOp pins the idempotent-close
// short circuit: a work item already in a Completed-category state gets no
// System.State patch op, only the status-tag update every close still sends.
func TestADOCloseWorkItemAlreadyCompletedIsNoOp(t *testing.T) {
	var patchBody []adoPatchOperation
	mux := http.NewServeMux()
	adoTestStates(t, mux, map[string][]map[string]string{
		"Issue": {
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Done", "category": "Completed"},
		},
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 42, "rev": 5, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue", "System.Title": "Fix",
					"System.State": "Done", "System.Tags": "goobers/status:claimed",
				},
			})
		case http.MethodPatch:
			decodeJSON(t, r, &patchBody)
			writeJSON(t, w, map[string]interface{}{
				"id": 42, "rev": 6, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue", "System.Title": "Fix",
					"System.State": "Done", "System.Tags": "goobers/status:done",
				},
			})
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, ID: "42", Status: WorkItemStatusDone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v", err)
	}
	if item.State != "closed" {
		t.Fatalf("state = %q, want closed", item.State)
	}
	for _, op := range patchBody {
		if op.Path == "/fields/System.State" {
			t.Fatalf("patch body = %#v, want no System.State op for an already-Completed item", patchBody)
		}
	}
}

// TestADOCloseWorkItemResolvedAdvancesToCompleted pins the middle step: a
// Bug sitting in Resolved (as transitionWorkItems or a "Fixes #" commit link
// would leave it) advances to the type's Completed state.
func TestADOCloseWorkItemResolvedAdvancesToCompleted(t *testing.T) {
	var patchBody []adoPatchOperation
	mux := http.NewServeMux()
	adoTestStates(t, mux, map[string][]map[string]string{
		"Bug": {
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Closed", "category": "Completed"},
		},
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/7", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 7, "rev": 2, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Bug", "System.Title": "Crash",
					"System.State": "Resolved", "System.Tags": "goobers/status:in-progress",
				},
			})
		case http.MethodPatch:
			decodeJSON(t, r, &patchBody)
			writeJSON(t, w, map[string]interface{}{
				"id": 7, "rev": 3, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Bug", "System.Title": "Crash",
					"System.State": "Closed", "System.Tags": "goobers/status:done",
				},
			})
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, ID: "7", Status: WorkItemStatusDone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v", err)
	}
	if item.State != "closed" {
		t.Fatalf("state = %q, want closed", item.State)
	}
	found := false
	for _, op := range patchBody {
		if op.Path == "/fields/System.State" && op.Value == "Closed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("patch body = %#v, want a System.State=Closed op", patchBody)
	}
}

// TestADOCloseWorkItemStopsAtResolvedWithoutCompletedState pins the
// no-Completed-transition case: an item already at Resolved on a type
// without a Completed state is success, left at Resolved, with a comment
// noting why instead of an error.
func TestADOCloseWorkItemStopsAtResolvedWithoutCompletedState(t *testing.T) {
	var stateOps int
	var comments []string
	mux := http.NewServeMux()
	adoTestStates(t, mux, map[string][]map[string]string{
		"Feedback Request": {
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
		},
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/9", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 9, "rev": 2, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Feedback Request", "System.Title": "Ask",
					"System.State": "Resolved", "System.Tags": "goobers/status:in-progress",
				},
			})
		case http.MethodPatch:
			var body []adoPatchOperation
			decodeJSON(t, r, &body)
			for _, op := range body {
				if op.Path == "/fields/System.State" {
					stateOps++
				}
			}
			writeJSON(t, w, map[string]interface{}{
				"id": 9, "rev": 3, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Feedback Request", "System.Title": "Ask",
					"System.State": "Resolved", "System.Tags": "goobers/status:done",
				},
			})
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/wit/workItems/9/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body struct {
			Text string `json:"text"`
		}
		decodeJSON(t, r, &body)
		comments = append(comments, body.Text)
		writeJSON(t, w, map[string]interface{}{"id": 1, "text": body.Text})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, ID: "9", Status: WorkItemStatusDone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v", err)
	}
	if item.State != "open" {
		t.Fatalf("state = %q, want open (still Resolved)", item.State)
	}
	if stateOps != 0 {
		t.Fatalf("System.State ops sent = %d, want 0", stateOps)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "Resolved") {
		t.Fatalf("comments = %#v, want one noting the Resolved stop", comments)
	}
}

// TestADOCloseWorkItemRetriesRevisionConflict pins the 412 handling: a
// revision conflict on the first close PATCH (attempting the System.State
// change) re-reads and retries; the re-read shows the item already Done —
// closed by a racing external write — so the retried PATCH carries no
// System.State op and succeeds.
func TestADOCloseWorkItemRetriesRevisionConflict(t *testing.T) {
	var stateOpsByAttempt []int
	mux := http.NewServeMux()
	adoTestStates(t, mux, map[string][]map[string]string{
		"Issue": {
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Done", "category": "Completed"},
		},
	})
	var gets, patches int
	mux.HandleFunc("/org/project/_apis/wit/workitems/11", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets++
			state := "Active"
			if gets > 1 {
				// The first PATCH's conflict means someone else closed it.
				state = "Done"
			}
			writeJSON(t, w, map[string]interface{}{
				"id": 11, "rev": gets, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue", "System.Title": "Race",
					"System.State": state, "System.Tags": "goobers/status:claimed",
				},
			})
		case http.MethodPatch:
			patches++
			var body []adoPatchOperation
			decodeJSON(t, r, &body)
			stateOps := 0
			for _, op := range body {
				if op.Path == "/fields/System.State" {
					stateOps++
				}
			}
			stateOpsByAttempt = append(stateOpsByAttempt, stateOps)
			if patches == 1 {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`{"message":"rev mismatch"}`))
				return
			}
			writeJSON(t, w, map[string]interface{}{
				"id": 11, "rev": gets + 1, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue", "System.Title": "Race",
					"System.State": "Done", "System.Tags": "goobers/status:done",
				},
			})
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, ID: "11", Status: WorkItemStatusDone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v", err)
	}
	if item.State != "closed" {
		t.Fatalf("state = %q, want closed", item.State)
	}
	if patches != 2 {
		t.Fatalf("patch attempts = %d, want exactly two (conflict, then a clean retry)", patches)
	}
	if len(stateOpsByAttempt) != 2 || stateOpsByAttempt[0] != 1 || stateOpsByAttempt[1] != 0 {
		t.Fatalf("System.State ops per attempt = %#v, want [1 0]: the retry finds it already Done", stateOpsByAttempt)
	}
}

// TestADOCloseWorkItemBoundedOnRepeatedConflict pins the failure mode: a
// close that conflicts on every retry attempt returns a bounded error
// instead of looping forever.
func TestADOCloseWorkItemBoundedOnRepeatedConflict(t *testing.T) {
	mux := http.NewServeMux()
	adoTestStates(t, mux, map[string][]map[string]string{
		"Issue": {
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Done", "category": "Completed"},
		},
	})
	var patches int
	mux.HandleFunc("/org/project/_apis/wit/workitems/13", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 13, "rev": 1, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue", "System.Title": "Stuck",
					"System.State": "Active", "System.Tags": "goobers/status:claimed",
				},
			})
		case http.MethodPatch:
			patches++
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"message":"rev mismatch"}`))
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, ID: "13", Status: WorkItemStatusDone,
	})
	if err == nil {
		t.Fatalf("UpdateWorkItemStatus: want a bounded error, got success")
	}
	if patches != adoClaimRetries {
		t.Fatalf("patch attempts = %d, want %d (bounded)", patches, adoClaimRetries)
	}
}
