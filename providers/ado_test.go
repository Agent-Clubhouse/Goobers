package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/labelpredicate"
)

func handleADOTestStateCategories(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"value": []map[string]string{
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Done", "category": "Completed"},
		}})
	})
}

func TestADOProviderOpenPullRequestCreatesThenUpdates(t *testing.T) {
	type requestBody struct {
		SourceRefName string `json:"sourceRefName"`
		TargetRefName string `json:"targetRefName"`
		Title         string `json:"title"`
		Description   string `json:"description"`
		IsDraft       *bool  `json:"isDraft"`
	}
	var posted, patched requestBody
	created := false
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			value := []interface{}{}
			if created {
				value = append(value, map[string]interface{}{
					"pullRequestId": 42,
					"url":           "api-pr-url",
					"sourceRefName": "refs/heads/goobers/implementation/run-1",
					"targetRefName": "refs/heads/main",
					"_links":        map[string]interface{}{"web": map[string]string{"href": "https://ado.example/pr/42"}},
				})
			}
			writeJSON(t, w, map[string]interface{}{"value": value})
		case http.MethodPost:
			decodeJSON(t, r, &posted)
			created = true
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId": 42,
				"url":           "api-pr-url",
				"_links":        map[string]interface{}{"web": map[string]string{"href": "https://ado.example/pr/42"}},
			})
		default:
			t.Fatalf("unexpected pullrequests method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		decodeJSON(t, r, &patched)
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId": 42,
			"url":           "api-pr-url",
			"_links":        map[string]interface{}{"web": map[string]string{"href": "https://ado.example/pr/42"}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Title:      "Implement ADO PR creation",
		Body:       "Provider-neutral open",
		Head:       "refs/heads/goobers/implementation/run-1",
		Base:       "refs/heads/main",
		Draft:      true,
	})
	if err != nil {
		t.Fatalf("OpenPullRequest returned error: %v", err)
	}
	if result.Number != 42 || result.ID != "42" || result.URL != "https://ado.example/pr/42" {
		t.Fatalf("OpenPullRequest result = %#v", result)
	}
	if posted.SourceRefName != "refs/heads/goobers/implementation/run-1" ||
		posted.TargetRefName != "refs/heads/main" ||
		posted.Title != "Implement ADO PR creation" ||
		posted.Description != "Provider-neutral open" ||
		posted.IsDraft == nil || !*posted.IsDraft {
		t.Fatalf("OpenPullRequest body = %#v", posted)
	}

	result, err = provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Title:      "Updated title",
		Body:       "Updated description",
		Head:       "goobers/implementation/run-1",
		Base:       "main",
		Draft:      false,
	})
	if err != nil {
		t.Fatalf("OpenPullRequest update returned error: %v", err)
	}
	if result.Number != 42 || result.ID != "42" || result.URL != "https://ado.example/pr/42" {
		t.Fatalf("OpenPullRequest update result = %#v", result)
	}
	if patched.Title != "Updated title" || patched.Description != "Updated description" ||
		patched.IsDraft == nil || *patched.IsDraft {
		t.Fatalf("OpenPullRequest update body = %#v", patched)
	}
}

func TestADOProviderOpenPullRequestReturnsProviderFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, map[string]interface{}{"value": []interface{}{}})
			return
		}
		http.Error(w, `{"message":"source branch does not exist"}`, http.StatusBadRequest)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Title:      "Broken",
		Head:       "missing",
		Base:       "main",
	})
	if err == nil || !strings.Contains(err.Error(), "source branch does not exist") {
		t.Fatalf("OpenPullRequest error = %v, want provider failure", err)
	}
}

func TestADOProviderMapsWorkItemsAndStatus(t *testing.T) {
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 42}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"id":  42,
			"rev": 3,
			"url": "https://dev.azure.com/org/project/_workitems/edit/42",
			"fields": map[string]interface{}{
				"System.WorkItemType": "User Story",
				"System.Title":        "Fix API",
				"System.Description":  "Make it pass",
				"System.State":        "Active",
				"System.Tags":         "route/backend; goobers/status:claimed",
				"System.AssignedTo":   map[string]interface{}{"displayName": "Mona"},
			},
			"relations": []map[string]interface{}{
				{"rel": "System.LinkTypes.Hierarchy-Reverse", "url": "https://dev.azure.com/org/_apis/wit/workItems/41"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Labels:     []string{"route/backend"},
		State:      "Active",
	})
	if err != nil {
		t.Fatalf("ListWorkItems returned error: %v", err)
	}

	if len(items) != 1 {
		t.Fatalf("len(items) = %d", len(items))
	}
	item := items[0]
	if item.Provider != ProviderADO || item.ID != "42" || item.Status != WorkItemStatusClaimed {
		t.Fatalf("unexpected item mapping: %#v", item)
	}
	if !item.HasLabel("route/backend") {
		t.Fatalf("expected scheduler routing label to be preserved: %#v", item.Labels)
	}
	if item.Parent == nil || item.Parent.Type != "parent" || item.Parent.ID != "41" {
		t.Fatalf("expected hierarchy parent to be preserved: %#v", item.Parent)
	}
}

func TestADOUpdateWorkItemAssignee(t *testing.T) {
	for _, tc := range []struct {
		name      string
		update    bool
		requested string
		want      string
	}{
		{name: "set", update: true, requested: "Octo Cat", want: "Octo Cat"},
		{name: "clear", update: true, requested: "", want: ""},
		{name: "nil leaves unchanged", want: "Mona"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patch []adoPatchOperation
			mux := http.NewServeMux()
			handleADOTestStateCategories(t, mux)
			mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
				fields := map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.Title":        "Fix",
					"System.State":        "Active",
				}
				switch r.Method {
				case http.MethodGet:
					fields["System.AssignedTo"] = map[string]interface{}{"displayName": "Mona"}
					writeJSON(t, w, map[string]interface{}{"id": 42, "rev": 3, "url": "item-url", "fields": fields})
				case http.MethodPatch:
					if !tc.update {
						t.Fatal("nil assignee must not PATCH")
					}
					decodeJSON(t, r, &patch)
					if tc.requested != "" {
						fields["System.AssignedTo"] = map[string]interface{}{"displayName": tc.requested}
					}
					// ADO 7.1's recorded "Reset an identity field" response omits
					// System.AssignedTo after adding an empty value.
					writeJSON(t, w, map[string]interface{}{"id": 42, "rev": 4, "url": "item-url", "fields": fields})
				default:
					t.Fatalf("unexpected work item method %s", r.Method)
				}
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			req := UpdateWorkItemRequest{
				Repository: RepositoryRef{Name: "repo", Project: "project"},
				ID:         "42",
			}
			if tc.update {
				assignee := tc.requested
				req.Assignee = &assignee
			}
			item, err := provider.UpdateWorkItem(context.Background(), req)
			if err != nil {
				t.Fatalf("UpdateWorkItem: %v", err)
			}
			if item.Assignee != tc.want {
				t.Fatalf("assignee = %q, want %q", item.Assignee, tc.want)
			}
			if !tc.update {
				if patch != nil {
					t.Fatalf("nil assignee patch = %#v, want none", patch)
				}
				return
			}
			if len(patch) != 2 ||
				patch[0].Op != "test" ||
				patch[0].Path != "/rev" ||
				patch[1].Op != "add" ||
				patch[1].Path != "/fields/System.AssignedTo" ||
				patch[1].Value != tc.requested {
				t.Fatalf("patch = %#v", patch)
			}
		})
	}
}

func TestADOListWorkItemsLimitCountsMatchingLabels(t *testing.T) {
	getRequests := 0
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != "" {
			t.Fatalf("$top = %q, want no raw candidate limit", got)
		}
		writeJSON(t, w, map[string]interface{}{
			"workItems": []map[string]int{{"id": 1}, {"id": 2}},
		})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
		getRequests++
		id := strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitems/")
		tags := "other"
		numericID := 1
		if id == "2" {
			tags = "wanted"
			numericID = 2
		}
		writeJSON(t, w, map[string]interface{}{
			"id": numericID,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.Title":        id,
				"System.State":        "New",
				"System.Tags":         tags,
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Labels:     []string{"wanted"},
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "2" || getRequests != 2 {
		t.Fatalf("items = %#v, GET requests = %d; want matching second work item", items, getRequests)
	}
}

// TestADOListWorkItemsOversizedScanFindsMatchBeyondTruncationBoundary is
// #2067's regression test: with Limit=1 and a LabelPredicate, the first
// WIQL candidate fails the predicate but a later one matches. Before the
// fix, $top was pinned to req.Limit (1), so the single fetched candidate
// was the nonmatching one and ListWorkItems returned zero items despite a
// real match existing — silently breaking "Limit = up to N matches". The
// fix gives the candidate fetch an oversized ceiling whenever a post-WIQL
// filter (here, LabelPredicate) is active, so the match is found in one
// call instead of requiring the caller to already know to page further.
func TestADOListWorkItemsOversizedScanFindsMatchBeyondTruncationBoundary(t *testing.T) {
	predicate, err := labelpredicate.Compile(`"wanted" in labels`, nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	getRequests := 0
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != strconv.Itoa(candidateScanCeiling) {
			t.Fatalf("$top = %q, want %d (oversized: LabelPredicate is active)", got, candidateScanCeiling)
		}
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}, {"id": 2}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
		getRequests++
		id := strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitems/")
		tags := "other"
		numericID := 1
		if id == "2" {
			tags = "wanted"
			numericID = 2
		}
		writeJSON(t, w, map[string]interface{}{
			"id": numericID,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.Title":        id,
				"System.State":        "New",
				"System.Tags":         tags,
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}

	pageInfo := &ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: repo, LabelPredicate: predicate, Limit: 1, PageInfo: pageInfo,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "2" || getRequests != 2 {
		t.Fatalf("items = %#v, GET requests = %d; want the matching second candidate found in one call", items, getRequests)
	}
	if pageInfo.HasNext {
		t.Fatalf("page info = %+v, want HasNext=false (every fetched candidate was scanned, and the fetch wasn't itself capped)", pageInfo)
	}
}

// TestADOListWorkItemsOversizedScanAppliesToStateFilterToo proves #2067's
// second acceptance criterion — "same guarantee holds for ... state
// filters on the bounded path" — not just LabelPredicate/FieldPredicate:
// requestedState "open"/"closed" needs each candidate's process-specific
// state category read before it can be compared, so it is exactly as
// unable to fold into WIQL's $top truncation as a predicate is.
func TestADOListWorkItemsOversizedScanAppliesToStateFilterToo(t *testing.T) {
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != strconv.Itoa(candidateScanCeiling) {
			t.Fatalf("$top = %q, want %d (oversized: a state filter is active)", got, candidateScanCeiling)
		}
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}, {"id": 2}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitems/")
		itemType, numericID := "Done", 1 // Completed category -> "closed", rejected by requestedState=open
		if id == "2" {
			itemType, numericID = "New", 2 // Proposed category -> "open", matches
		}
		writeJSON(t, w, map[string]interface{}{
			"id": numericID,
			"fields": map[string]interface{}{
				"System.WorkItemType": itemType,
				"System.Title":        id,
				"System.State":        itemType,
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}

	pageInfo := &ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: repo, State: "open", Limit: 1, PageInfo: pageInfo,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "2" {
		t.Fatalf("items = %#v; want the open second candidate found in one call", items)
	}
	if pageInfo.HasNext {
		t.Fatalf("page info = %+v, want HasNext=false", pageInfo)
	}
}

func TestADOListWorkItemsProjectsAndFiltersNativeFields(t *testing.T) {
	predicate, err := fieldpredicate.Compile(`fields["Microsoft.VSTS.Common.Priority"] <= 2`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}, {"id": 2}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitems/")
		numericID := 1
		priority := 3
		if id == "2" {
			numericID = 2
			priority = 1
		}
		writeJSON(t, w, map[string]interface{}{
			"id": numericID,
			"fields": map[string]interface{}{
				"System.Title":                   "item " + id,
				"System.WorkItemType":            "Issue",
				"System.State":                   "Active",
				"Microsoft.VSTS.Common.Priority": priority,
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository:     RepositoryRef{Name: "repo", Project: "project"},
		FieldPredicate: predicate,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "2" {
		t.Fatalf("items = %#v, want work item 2", items)
	}
	if got := items[0].Fields["Microsoft.VSTS.Common.Priority"]; got != float64(1) {
		t.Fatalf("priority = %#v, want float64(1)", got)
	}
}

func TestADOListWorkItemsUnavailableNativeFieldFails(t *testing.T) {
	predicate, err := fieldpredicate.Compile(`fields["Custom.Risk"] == "high"`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"id": 1,
			"fields": map[string]interface{}{
				"System.Title":        "item",
				"System.WorkItemType": "Issue",
				"System.State":        "Active",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })

	_, err = provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository:     RepositoryRef{Name: "repo", Project: "project"},
		FieldPredicate: predicate,
	})
	if err == nil || !strings.Contains(err.Error(), `field "Custom.Risk" is unavailable`) {
		t.Fatalf("ListWorkItems error = %v, want unavailable-field error", err)
	}
}

func TestADOProviderRepoAndBacklogOperations(t *testing.T) {
	var patchBody []adoPatchOperation
	var reviewerPath string
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/refs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{{"name": "refs/heads/work", "objectId": "branch-tip", "url": "ref-url"}}})
		case http.MethodPost:
			writeJSON(t, w, map[string]interface{}{"value": []map[string]string{{"name": "refs/heads/work", "objectId": "base-sha", "url": "ref-url"}}})
		default:
			t.Fatalf("unexpected refs method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pushes", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body adoPushRequest
		decodeJSON(t, r, &body)
		if len(body.RefUpdates) != 1 || body.RefUpdates[0].OldObjectID != "branch-tip" {
			t.Fatalf("expected current branch tip in ref update, got %#v", body.RefUpdates)
		}
		if len(body.Commits) != 1 || len(body.Commits[0].Changes) != 2 {
			t.Fatalf("expected two changes, got %#v", body)
		}
		if body.Commits[0].Changes[0].ChangeType != "edit" || body.Commits[0].Changes[1].ChangeType != "delete" {
			t.Fatalf("expected edit change for existing file, got %#v", body)
		}
		if body.Commits[0].Changes[1].NewContent != nil {
			t.Fatalf("delete change should not include newContent: %#v", body.Commits[0].Changes[1])
		}
		writeJSON(t, w, map[string]interface{}{"url": "push-url", "commits": []map[string]string{{"commitId": "commit-sha"}}})
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/items", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Idempotent OpenPullRequest lists active PRs first; no PR exists yet.
			writeJSON(t, w, map[string]interface{}{"value": []interface{}{}})
		case http.MethodPost:
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 12, "url": "pr-url"})
		default:
			t.Fatalf("unexpected pullrequests method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12/reviewers/qa-1", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPut)
		reviewerPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 42, "rev": 3, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.Title":        "Fix",
					"System.State":        "Active",
					"System.Tags":         "route/backend; goobers/status:claimed",
				},
			})
		case http.MethodPatch:
			if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json-patch+json") {
				t.Fatalf("Content-Type = %q", got)
			}
			decodeJSON(t, r, &patchBody)
			writeJSON(t, w, map[string]interface{}{
				"id": 42, "rev": 4, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.Title":        "Fix",
					"System.State":        "Active",
					"System.Tags":         "route/backend; goobers/status:in-progress",
				},
			})
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	if branch, err := provider.CreateBranch(context.Background(), BranchRequest{Repository: repo, BaseSHA: "base-sha", Name: "work"}); err != nil || branch.Name != "work" {
		t.Fatalf("CreateBranch = %#v, %v", branch, err)
	}
	files := []CommitFile{
		{Path: "README.md", Content: "hello"},
		{Path: "old.txt", ChangeType: string(CommitChangeDelete)},
	}
	if commit, err := provider.Commit(context.Background(), CommitRequest{Repository: repo, Branch: "work", Message: "docs", Files: files}); err != nil || commit.SHA != "commit-sha" {
		t.Fatalf("Commit = %#v, %v", commit, err)
	}
	pr, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{Repository: repo, Title: "Fix", Head: "work", Base: "main"})
	if err != nil || pr.Number != 12 {
		t.Fatalf("OpenPullRequest = %#v, %v", pr, err)
	}
	if err := provider.RequestReview(context.Background(), ReviewRequest{Repository: repo, PullID: "12", Reviewers: []string{"qa-1"}}); err != nil {
		t.Fatalf("RequestReview returned error: %v", err)
	}
	if reviewerPath != "/org/project/_apis/git/repositories/repo/pullrequests/12/reviewers/qa-1" {
		t.Fatalf("reviewer path = %q", reviewerPath)
	}
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{Repository: repo, ID: "42", Status: WorkItemStatusInProgress})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus returned error: %v", err)
	}
	if item.Status != WorkItemStatusInProgress {
		t.Fatalf("updated item status = %q", item.Status)
	}
	if len(patchBody) != 2 ||
		patchBody[0].Op != "test" ||
		patchBody[0].Path != "/rev" ||
		patchBody[1].Path != "/fields/System.Tags" ||
		patchBody[1].Value != "route/backend; goobers/status:in-progress" {
		t.Fatalf("patch body = %#v", patchBody)
	}
}

func TestADOProviderListPullRequests(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		if got := r.Header.Get("Authorization"); got != basicAuth("goobers", "token") {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.URL.Query().Get("api-version"); got != "7.1" {
			t.Fatalf("api-version = %q", got)
		}
		if got := r.URL.Query().Get("searchCriteria.status"); got != "active" {
			t.Fatalf("searchCriteria.status = %q", got)
		}
		if got := r.URL.Query().Get("searchCriteria.includeLinks"); got != "true" {
			t.Fatalf("searchCriteria.includeLinks = %q", got)
		}
		if got := r.URL.Query().Get("searchCriteria.targetRefName"); got != "refs/heads/main" {
			t.Fatalf("searchCriteria.targetRefName = %q", got)
		}
		if got := r.URL.Query().Get("$top"); got != "100" {
			t.Fatalf("$top = %q", got)
		}
		if got := r.URL.Query().Get("$skip"); got != "0" {
			t.Fatalf("$skip = %q", got)
		}
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
			{
				"pullRequestId": 12,
				"url":           "api-pr-url",
				"status":        "active",
				"title":         "Implement ADO reads",
				"createdBy":     map[string]string{"displayName": "Mona", "uniqueName": "mona@example.com"},
				"reviewers": []map[string]interface{}{
					{"displayName": "Pending Reviewer", "uniqueName": "reviewer@example.com", "vote": 0},
					{"displayName": "Completed Reviewer", "uniqueName": "done@example.com", "vote": 10},
				},
				"creationDate":          "2026-07-15T20:30:00Z",
				"sourceRefName":         "refs/heads/goobers/implementation/run-1",
				"targetRefName":         "refs/heads/main",
				"isDraft":               true,
				"labels":                []map[string]string{{"name": "goobers:needs-remediation"}},
				"lastMergeSourceCommit": map[string]string{"commitId": "head-sha"},
				"lastMergeTargetCommit": map[string]string{"commitId": "base-sha"},
				"_links":                map[string]interface{}{"web": map[string]string{"href": "web-pr-url"}},
			},
			{
				"pullRequestId": 13,
				"sourceRefName": "refs/heads/goobers/implementation/run-2",
				"targetRefName": "refs/heads/main",
				"createdBy":     map[string]string{"uniqueName": "other@example.com"},
				"reviewers":     []map[string]interface{}{{"uniqueName": "reviewer@example.com", "vote": 0}},
			},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	prs, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository:        RepositoryRef{Name: "repo", Project: "project"},
		Base:              "main",
		HeadPrefix:        "goobers/",
		Author:            "mona@example.com",
		RequestedReviewer: "reviewer@example.com",
	})
	if err != nil {
		t.Fatalf("ListPullRequests returned error: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("len(prs) = %d, want 1: %#v", len(prs), prs)
	}
	pr := prs[0]
	if pr.ID != "12" || pr.Number != 12 || pr.URL != "web-pr-url" {
		t.Fatalf("unexpected pull request identity: %#v", pr)
	}
	if pr.Head != "goobers/implementation/run-1" || pr.Base != "main" || pr.HeadSHA != "head-sha" || pr.BaseSHA != "base-sha" {
		t.Fatalf("unexpected pull request refs: %#v", pr)
	}
	if !pr.Draft || pr.CheckState != CheckStatePending || len(pr.Labels) != 1 || pr.Labels[0] != "goobers:needs-remediation" {
		t.Fatalf("unexpected pull request metadata: %#v", pr)
	}
	if pr.Author != "mona@example.com" || len(pr.Assignees) != 0 ||
		len(pr.RequestedReviewers) != 1 || pr.RequestedReviewers[0] != "reviewer@example.com" {
		t.Fatalf("unexpected pull request identities: %#v", pr)
	}
	if got := pr.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"); got != "2026-07-15T20:30:00Z" {
		t.Fatalf("UpdatedAt = %q", got)
	}
}

func TestADOProviderListPullRequestsRejectsAssigneeFilter(t *testing.T) {
	provider := NewADOProvider("org", "project", "token")
	_, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Assignee:   "mona@example.com",
	})
	var unsupported ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.Capability != CapPRQueryAssignee {
		t.Fatalf("ListPullRequests error = %v, want ErrUnsupported for %q", err, CapPRQueryAssignee)
	}
}

func TestADOProviderPullRequestFiles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12/iterations", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		if got := r.Header.Get("Authorization"); got != basicAuth("goobers", "token") {
			t.Fatalf("Authorization = %q", got)
		}
		writeJSON(t, w, map[string]interface{}{"value": []map[string]int{{"id": 1}, {"id": 3}, {"id": 2}}})
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12/iterations/3/changes", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		switch got := r.URL.Query().Get("$skip"); got {
		case "0":
			if top := r.URL.Query().Get("$top"); top != "2000" {
				t.Fatalf("first $top = %q", top)
			}
			writeJSON(t, w, map[string]interface{}{
				"changeEntries": []map[string]interface{}{
					{"changeType": "add", "item": map[string]string{"path": "/cmd/goobers/new.go"}},
					{"changeType": "edit", "item": map[string]string{"path": "/internal/runner/run.go"}},
				},
				"nextSkip": 2,
				"nextTop":  2,
			})
		case "2":
			if top := r.URL.Query().Get("$top"); top != "2" {
				t.Fatalf("second $top = %q", top)
			}
			writeJSON(t, w, map[string]interface{}{
				"changeEntries": []map[string]interface{}{
					{"changeType": "delete", "item": map[string]string{"path": "/old.txt"}},
					{"changeType": "rename", "item": map[string]string{"path": "/new-name.txt"}},
				},
				"nextSkip": 0,
				"nextTop":  0,
			})
		default:
			t.Fatalf("unexpected $skip = %q", got)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	files, err := provider.PullRequestFiles(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "12")
	if err != nil {
		t.Fatalf("PullRequestFiles returned error: %v", err)
	}
	if len(files) != 4 {
		t.Fatalf("len(files) = %d, want 4: %#v", len(files), files)
	}
	want := []ChangedFile{
		{Path: "cmd/goobers/new.go", Status: "added", Integrity: apiintegrity.Unapproved},
		{Path: "internal/runner/run.go", Status: "modified", Integrity: apiintegrity.Unapproved},
		{Path: "old.txt", Status: "removed", Integrity: apiintegrity.Unapproved},
		{Path: "new-name.txt", Status: "renamed", Integrity: apiintegrity.Unapproved},
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("files[%d] = %#v, want %#v", i, files[i], want[i])
		}
	}
}

func TestADOProviderCreateWorkItemSubscribeAndClone(t *testing.T) {
	var wiqlCalls int
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/workitems/$Issue", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var patch []adoPatchOperation
		decodeJSON(t, r, &patch)
		if len(patch) < 3 || patch[0].Value != "New work" || patch[2].Value != "route/backend; goobers/status:claimed" {
			t.Fatalf("unexpected create patch: %#v", patch)
		}
		writeJSON(t, w, map[string]interface{}{
			"id": 51, "rev": 1, "url": "item-url",
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.Title":        "New work",
				"System.State":        "New",
				"System.Tags":         "route/backend; goobers/status:claimed",
			},
		})
	})
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		wiqlCalls++
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 50 + wiqlCalls}}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/51", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"id": 51, "rev": wiqlCalls, "url": "item-url",
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.Title":        "New work",
				"System.State":        "New",
				"System.Tags":         "route/backend",
			},
		})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/52", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"id": 52, "rev": wiqlCalls, "url": "item-url",
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.Title":        "New work 2",
				"System.State":        "New",
				"System.Tags":         "route/backend",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	runner := &adoAuthRunner{}
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) {
		p.BaseURL = server.URL
		p.Runner = runner
	})
	repo := RepositoryRef{Name: "repo", Project: "project"}
	item, err := provider.CreateWorkItem(context.Background(), CreateWorkItemRequest{
		Repository: repo,
		Title:      "New work",
		Labels:     []string{"route/backend"},
		Status:     WorkItemStatusClaimed,
	})
	if err != nil || item.ID != "51" || item.Status != WorkItemStatusClaimed {
		t.Fatalf("CreateWorkItem = %#v, %v", item, err)
	}
	if provider.Kind() != ProviderADO {
		t.Fatalf("Kind = %q", provider.Kind())
	}
	clone, err := provider.CloneRepository(context.Background(), CloneRequest{Repository: repo, Destination: "/tmp/app", Branch: "main"})
	if err != nil {
		t.Fatalf("CloneRepository returned error: %v", err)
	}
	if clone.Path != "/tmp/app" || !strings.Contains(strings.Join(runner.args, " "), "clone") {
		t.Fatalf("unexpected clone result=%#v args=%#v", clone, runner.args)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := provider.Subscribe(ctx, TriggerSubscription{Kind: TriggerPolling, Repository: repo, PollInterval: 1})
	if err != nil {
		t.Fatalf("Subscribe returned error: %v", err)
	}
	first := <-events
	second := <-events
	if first.Item.ID == second.Item.ID {
		t.Fatalf("expected polling subscription to continue and emit changed items, got %q twice", first.Item.ID)
	}
}

func TestADOProviderErrorPaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	if _, err := provider.GetWorkItem(context.Background(), repo, "42"); err == nil {
		t.Fatal("expected non-2xx response to return an error")
	}
	if _, err := provider.CreateBranch(context.Background(), BranchRequest{Repository: repo}); err == nil {
		t.Fatal("expected missing branch name to return an error")
	}
	if _, err := provider.Subscribe(context.Background(), TriggerSubscription{Kind: TriggerWebhook, Repository: repo}); err == nil {
		t.Fatal("expected unsupported webhook subscription to return an error")
	}
	if _, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{}); err == nil {
		t.Fatal("expected missing repository to return an error")
	}
	if _, err := provider.PullRequestFiles(context.Background(), repo, ""); err == nil {
		t.Fatal("expected missing pull id to return an error")
	}
}
