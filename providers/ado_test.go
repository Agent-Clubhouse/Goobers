package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestADOListWorkItemsBoundsAndAdvancesPredicateScan(t *testing.T) {
	predicate, err := labelpredicate.Compile(`"wanted" in labels`, nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	getRequests := 0
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != "1" {
			t.Fatalf("$top = %q, want 1", got)
		}
		var body map[string]string
		decodeJSON(t, r, &body)
		id := 1
		if strings.Contains(body["query"], "[System.Id] > 1") {
			id = 2
		}
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": id}}})
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

	firstPage := &ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: repo, LabelPredicate: predicate, Limit: 1, PageInfo: firstPage,
	})
	if err != nil {
		t.Fatalf("ListWorkItems page 1: %v", err)
	}
	if len(items) != 0 || getRequests != 1 || !firstPage.HasNext || firstPage.NextCursor != "1" {
		t.Fatalf(
			"items = %#v, GET requests = %d, page info = %+v; want one nonmatching raw candidate",
			items, getRequests, firstPage,
		)
	}
	pageInfo := &ListWorkItemsPageInfo{}
	items, err = provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: repo, LabelPredicate: predicate, Limit: 1, Cursor: "1", PageInfo: pageInfo,
	})
	if err != nil {
		t.Fatalf("ListWorkItems page 2: %v", err)
	}
	if len(items) != 1 || items[0].ID != "2" || getRequests != 2 {
		t.Fatalf("items = %#v, GET requests = %d; want matching second candidate", items, getRequests)
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
			writeJSON(t, w, map[string]interface{}{"value": []interface{}{}})
		case http.MethodPost:
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 12, "url": "pr-url"})
		default:
			t.Fatalf("unexpected pull request method %s", r.Method)
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

func TestADOProviderCreatesPullRequest(t *testing.T) {
	var body map[string]interface{}
	recorder := &recordingRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			query := r.URL.Query()
			if query.Get("searchCriteria.status") != "active" ||
				query.Get("searchCriteria.sourceRefName") != "refs/heads/goobers/implementation/run-1" ||
				query.Get("searchCriteria.targetRefName") != "refs/heads/main" ||
				query.Get("searchCriteria.includeLinks") != "true" ||
				query.Get("$top") != "1" {
				t.Fatalf("unexpected pull request lookup query: %s", r.URL.RawQuery)
			}
			writeJSON(t, w, map[string]interface{}{"value": []interface{}{}})
		case http.MethodPost:
			decodeJSON(t, r, &body)
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId": 12,
				"url":           "api-pr-url",
				"_links":        map[string]interface{}{"web": map[string]string{"href": "web-pr-url"}},
			})
		default:
			t.Fatalf("unexpected pull request method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider(
		"org",
		"project",
		"token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADOMutationRecorder(recorder),
	)
	result, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Title:      "Implement ADO parity",
		Body:       "Provider contract",
		Head:       "refs/heads/goobers/implementation/run-1",
		Base:       "main",
		Draft:      true,
		RunID:      "run-1",
	})
	if err != nil {
		t.Fatalf("OpenPullRequest returned error: %v", err)
	}
	if result.ID != "12" || result.Number != 12 || result.URL != "web-pr-url" {
		t.Fatalf("result = %#v", result)
	}
	if body["sourceRefName"] != "refs/heads/goobers/implementation/run-1" || body["targetRefName"] != "refs/heads/main" {
		t.Fatalf("pull request refs = %#v", body)
	}
	if body["title"] != "Implement ADO parity" || body["isDraft"] != true {
		t.Fatalf("pull request metadata = %#v", body)
	}
	description, _ := body["description"].(string)
	if !strings.Contains(description, "Provider contract") || !strings.Contains(description, "run-1") {
		t.Fatalf("description = %q", description)
	}
	ref, ok := recorder.last()
	if !ok || ref.Provider != ProviderADO || ref.Ref != "org/project/repo#12" ||
		ref.Operation != "open" || ref.RunID != "run-1" || ref.URL != "web-pr-url" {
		t.Fatalf("mutation = %#v", ref)
	}
	if ref.Fields["description"].After != digestString(description) {
		t.Fatalf("description mutation = %#v", ref.Fields["description"])
	}
}

func TestADOProviderUpdatesExistingPullRequestOnRepass(t *testing.T) {
	var patchBody map[string]interface{}
	postCalls := 0
	patchCalls := 0
	active := false
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if !active {
				writeJSON(t, w, map[string]interface{}{"value": []interface{}{}})
				return
			}
			writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{{
				"pullRequestId": 12,
				"url":           "api-pr-url",
				"sourceRefName": "refs/heads/goobers/implementation/run-1",
				"targetRefName": "refs/heads/main",
				"_links":        map[string]interface{}{"web": map[string]string{"href": "web-pr-url"}},
			}}})
		case http.MethodPost:
			postCalls++
			active = true
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId": 12,
				"url":           "api-pr-url",
				"_links":        map[string]interface{}{"web": map[string]string{"href": "web-pr-url"}},
			})
		default:
			t.Fatalf("unexpected pull request method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		patchCalls++
		decodeJSON(t, r, &patchBody)
		writeJSON(t, w, map[string]interface{}{"pullRequestId": 12})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	request := PullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Title:      "Initial title",
		Body:       "Initial body",
		Head:       "goobers/implementation/run-1",
		Base:       "main",
		Draft:      true,
		RunID:      "run-1",
	}
	first, err := provider.OpenPullRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("first OpenPullRequest returned error: %v", err)
	}
	request.Title = "Updated title"
	request.Body = "Updated body"
	result, err := provider.OpenPullRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("repass OpenPullRequest returned error: %v", err)
	}
	if first.ID != "12" || result.ID != first.ID || result.Number != first.Number || result.URL != first.URL {
		t.Fatalf("first result = %#v, repass result = %#v", first, result)
	}
	if postCalls != 1 || patchCalls != 1 {
		t.Fatalf("POST calls = %d, PATCH calls = %d; want one creation and one repass update", postCalls, patchCalls)
	}
	if patchBody["title"] != "Updated title" || patchBody["isDraft"] != true {
		t.Fatalf("patch body = %#v", patchBody)
	}
	description, _ := patchBody["description"].(string)
	if !strings.Contains(description, "Updated body") || !strings.Contains(description, "goobers run-id: run-1") {
		t.Fatalf("description = %q", description)
	}
}

func TestADOProviderBoundsPullRequestDescriptionOnCreateAndUpdate(t *testing.T) {
	var descriptions []string
	active := false
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if active {
				writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{{"pullRequestId": 12}}})
			} else {
				writeJSON(t, w, map[string]interface{}{"value": []interface{}{}})
			}
		case http.MethodPost:
			var body map[string]interface{}
			decodeJSON(t, r, &body)
			descriptions = append(descriptions, body["description"].(string))
			active = true
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 12})
		default:
			t.Fatalf("unexpected pull request method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		var body map[string]interface{}
		decodeJSON(t, r, &body)
		descriptions = append(descriptions, body["description"].(string))
		writeJSON(t, w, map[string]interface{}{"pullRequestId": 12})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	request := PullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Title:      "Bound the description",
		Body:       strings.Repeat("🚀", adoPullRequestDescriptionLimit),
		Head:       "goobers/implementation/run-1",
		Base:       "main",
		RunID:      "run-1",
	}
	if _, err := provider.OpenPullRequest(context.Background(), request); err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	request.Body += "updated"
	if _, err := provider.OpenPullRequest(context.Background(), request); err != nil {
		t.Fatalf("update pull request: %v", err)
	}

	if len(descriptions) != 2 {
		t.Fatalf("descriptions = %d, want create and update", len(descriptions))
	}
	suffix := "\n\n---\n" + runFooter(request.RunID)
	for i, description := range descriptions {
		if got := utf16CodeUnits(description); got > adoPullRequestDescriptionLimit ||
			got < adoPullRequestDescriptionLimit-1 {
			t.Errorf("description %d UTF-16 length = %d, want at most %d with no more than one unused unit", i, got, adoPullRequestDescriptionLimit)
		}
		if !utf8.ValidString(description) {
			t.Errorf("description %d is not valid UTF-8", i)
		}
		if !strings.HasSuffix(description, suffix) {
			t.Errorf("description %d does not preserve run footer", i)
		}
	}
}

func TestADOProviderPollPullRequestMapsReviewsAndBuilds(t *testing.T) {
	since := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId":         12,
			"url":                   "api-pr-url",
			"status":                "active",
			"title":                 "Implement ADO parity",
			"description":           "Provider contract",
			"sourceRefName":         "refs/heads/goobers/implementation/run-1",
			"targetRefName":         "refs/heads/main",
			"isDraft":               true,
			"mergeStatus":           "succeeded",
			"reviewers":             []map[string]int{{"vote": 10}, {"vote": -5}, {"vote": 0}},
			"repository":            map[string]string{"id": "11111111-2222-3333-4444-555555555555"},
			"lastMergeSourceCommit": map[string]string{"commitId": "head-sha"},
			"lastMergeTargetCommit": map[string]string{"commitId": "base-sha"},
			"_links":                map[string]interface{}{"web": map[string]string{"href": "web-pr-url"}},
		})
	})
	mux.HandleFunc("/org/project/_apis/build/builds", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		query := r.URL.Query()
		if query.Get("api-version") != "7.1" ||
			query.Get("branchName") != "refs/pull/12/merge" ||
			query.Get("reasonFilter") != "pullRequest" ||
			query.Get("repositoryId") != "11111111-2222-3333-4444-555555555555" ||
			query.Get("repositoryType") != "TfsGit" ||
			query.Get("queryOrder") != "queueTimeDescending" ||
			query.Get("$top") != "100" {
			t.Fatalf("unexpected build query: %s", r.URL.RawQuery)
		}
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
			{
				"id": 21, "buildNumber": "20260726.2", "status": "completed", "result": "succeeded",
				"definition":  map[string]interface{}{"id": 7, "name": "provider-ci"},
				"triggerInfo": map[string]string{"pr.sourceSha": "head-sha"},
				"_links":      map[string]interface{}{"web": map[string]string{"href": "build-url"}},
			},
			{
				"id": 20, "buildNumber": "20260726.1", "status": "completed", "result": "failed",
				"definition":  map[string]interface{}{"id": 7, "name": "provider-ci"},
				"triggerInfo": map[string]string{"pr.sourceSha": "head-sha"},
			},
			{
				"id": 22, "buildNumber": "20260726.3", "status": "completed", "result": "partiallySucceeded",
				"definition":  map[string]interface{}{"id": 8, "name": "lint"},
				"triggerInfo": map[string]string{"pr.sourceSha": "head-sha"},
			},
			{
				"id": 23, "buildNumber": "20260726.4", "status": "completed", "result": "failed",
				"definition":  map[string]interface{}{"id": 9, "name": "stale"},
				"triggerInfo": map[string]string{"pr.sourceSha": "superseded-sha"},
			},
		}})
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12/threads", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
			{
				"id": 4,
				"comments": []map[string]interface{}{
					{
						"id": 1, "content": "before cutoff", "publishedDate": since.Add(-time.Minute),
						"author": map[string]string{"uniqueName": "old@example.com"},
					},
					{
						"id": 2, "content": "please update", "publishedDate": since.Add(time.Minute),
						"author": map[string]string{"uniqueName": "reviewer@example.com"},
					},
					{
						"id": 3, "content": "deleted", "publishedDate": since.Add(2 * time.Minute), "isDeleted": true,
					},
				},
			},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
		Repository:    RepositoryRef{Name: "repo", Project: "project"},
		PullID:        "12",
		CommentsSince: &since,
	})
	if err != nil {
		t.Fatalf("PollPullRequest returned error: %v", err)
	}
	if result.Number != 12 || result.State != "open" || result.Merged || result.URL != "web-pr-url" {
		t.Fatalf("pull request identity/state = %#v", result)
	}
	if result.Mergeable == nil || !*result.Mergeable || result.MergeableState != "succeeded" {
		t.Fatalf("mergeability = %#v, %q", result.Mergeable, result.MergeableState)
	}
	if result.HeadBranch != "goobers/implementation/run-1" || result.BaseBranch != "main" ||
		result.HeadSHA != "head-sha" || result.BaseSHA != "base-sha" || result.Body != "Provider contract" {
		t.Fatalf("pull request refs/body = %#v", result)
	}
	if result.HeadRepository == nil || result.HeadRepository.Provider != ProviderADO ||
		result.HeadRepository.Owner != "org" || result.HeadRepository.Project != "project" {
		t.Fatalf("head repository = %#v", result.HeadRepository)
	}
	if result.ReviewDecision != ReviewDecisionChangesRequested || result.RequestedChanges != 1 {
		t.Fatalf("review state = %q, requested changes = %d", result.ReviewDecision, result.RequestedChanges)
	}
	if result.CheckState != CheckStateFailing || len(result.Checks) != 2 {
		t.Fatalf("check state = %q, checks = %#v", result.CheckState, result.Checks)
	}
	if result.Checks[0].Name != "provider-ci" || result.Checks[0].URL != "build-url" ||
		result.Checks[1].State != CheckStateFailing {
		t.Fatalf("checks = %#v", result.Checks)
	}
	if len(result.CommentsSince) != 1 || result.CommentsSince[0].Body != "please update" ||
		result.CommentsSince[0].Author != "reviewer@example.com" {
		t.Fatalf("comments since = %#v", result.CommentsSince)
	}
}

func TestADOProviderPullRequestBuildStateRequiresCurrentHeadCorrelation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/build/builds", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
			{
				"id": 23, "buildNumber": "20260726.4", "status": "completed", "result": "succeeded",
				"definition": map[string]interface{}{"id": 7, "name": "provider-ci"},
			},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	state, checks, err := provider.pullRequestBuildState(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		"12",
		"repo-id",
		"new-head-sha",
	)
	if err != nil {
		t.Fatalf("pullRequestBuildState returned error: %v", err)
	}
	if state != CheckStatePending || len(checks) != 0 {
		t.Fatalf("build state = %q, checks = %#v, want pending with no correlated builds", state, checks)
	}
}

func TestADOProviderPollPullRequestMapsTerminalStates(t *testing.T) {
	closedAt := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		status string
		merged bool
	}{
		{status: "abandoned"},
		{status: "completed", merged: true},
	} {
		t.Run(test.status, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, map[string]interface{}{
					"pullRequestId": 12,
					"status":        test.status,
					"closedDate":    closedAt.Format(time.RFC3339),
					"repository":    map[string]string{"id": "11111111-2222-3333-4444-555555555555"},
				})
			})
			mux.HandleFunc("/org/project/_apis/build/builds", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, map[string]interface{}{"value": []interface{}{}})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			result, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
				Repository: RepositoryRef{Name: "repo", Project: "project"},
				PullID:     "12",
			})
			if err != nil {
				t.Fatalf("PollPullRequest returned error: %v", err)
			}
			if result.State != "closed" || result.Merged != test.merged {
				t.Fatalf("result state = %q, merged = %t", result.State, result.Merged)
			}
			if test.merged && (result.MergedAt == nil || !result.MergedAt.Equal(closedAt)) {
				t.Fatalf("MergedAt = %v, want %v", result.MergedAt, closedAt)
			}
			if !test.merged && result.MergedAt != nil {
				t.Fatalf("MergedAt = %v, want nil for abandoned pull request", result.MergedAt)
			}
		})
	}
}

func TestADOProviderReviewBuildAndTerminalMappings(t *testing.T) {
	reviewTests := []struct {
		name      string
		votes     []int
		decision  ReviewDecision
		requested int
	}{
		{name: "no votes", votes: nil, decision: ReviewDecisionPending},
		{name: "approved", votes: []int{0, 5, 10}, decision: ReviewDecisionApproved},
		{name: "waiting for author", votes: []int{10, -5}, decision: ReviewDecisionChangesRequested, requested: 1},
		{name: "rejected", votes: []int{-10, -5}, decision: ReviewDecisionChangesRequested, requested: 2},
	}
	for _, test := range reviewTests {
		t.Run("review "+test.name, func(t *testing.T) {
			reviewers := make([]adoReviewer, len(test.votes))
			for i, vote := range test.votes {
				reviewers[i].Vote = vote
			}
			decision, requested := adoReviewDecision(reviewers)
			if decision != test.decision || requested != test.requested {
				t.Fatalf("adoReviewDecision(%v) = %q, %d; want %q, %d", test.votes, decision, requested, test.decision, test.requested)
			}
		})
	}

	buildTests := []struct {
		status string
		result string
		want   CheckState
	}{
		{status: "notStarted", want: CheckStatePending},
		{status: "inProgress", want: CheckStatePending},
		{status: "completed", result: "succeeded", want: CheckStatePassing},
		{status: "completed", result: "partiallySucceeded", want: CheckStateFailing},
		{status: "completed", result: "failed", want: CheckStateFailing},
		{status: "completed", result: "canceled", want: CheckStateFailing},
		{status: "completed", result: "none", want: CheckStatePending},
	}
	for _, test := range buildTests {
		t.Run("build "+test.status+" "+test.result, func(t *testing.T) {
			if got := adoBuildState(test.status, test.result); got != test.want {
				t.Fatalf("adoBuildState(%q, %q) = %q, want %q", test.status, test.result, got, test.want)
			}
		})
	}

	stateTests := []struct {
		status string
		state  string
		merged bool
	}{
		{status: "active", state: "open"},
		{status: "abandoned", state: "closed"},
		{status: "completed", state: "merged", merged: true},
	}
	for _, test := range stateTests {
		t.Run("terminal "+test.status, func(t *testing.T) {
			state, merged := adoPullRequestState(test.status)
			if state != test.state || merged != test.merged {
				t.Fatalf("adoPullRequestState(%q) = %q, %t; want %q, %t", test.status, state, merged, test.state, test.merged)
			}
		})
	}

	pollStateTests := []struct {
		status string
		state  string
		merged bool
	}{
		{status: "active", state: "open"},
		{status: "abandoned", state: "closed"},
		{status: "completed", state: "closed", merged: true},
	}
	for _, test := range pollStateTests {
		t.Run("poll terminal "+test.status, func(t *testing.T) {
			state, merged := adoPullRequestPollState(test.status)
			if state != test.state || merged != test.merged {
				t.Fatalf("adoPullRequestPollState(%q) = %q, %t; want %q, %t", test.status, state, merged, test.state, test.merged)
			}
		})
	}
}

func TestADOProviderClosePullRequestAbandonsAndComments(t *testing.T) {
	var patches int
	var patchBody map[string]string
	var threadBody adoPullRequestThreadRequest
	recorder := &recordingRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 12, "status": "active"})
		case http.MethodPatch:
			patches++
			decodeJSON(t, r, &patchBody)
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 12, "status": "abandoned"})
		default:
			t.Fatalf("unexpected pull request method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12/threads", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		decodeJSON(t, r, &threadBody)
		w.WriteHeader(http.StatusCreated)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider(
		"org",
		"project",
		"token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADOMutationRecorder(recorder),
	)
	result, err := provider.ClosePullRequest(context.Background(), ClosePullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "12",
		Comment:    "No longer needed",
	})
	if err != nil {
		t.Fatalf("ClosePullRequest returned error: %v", err)
	}
	if result.Number != 12 || result.Merged || result.State != "closed" {
		t.Fatalf("result = %#v", result)
	}
	if patches != 1 || patchBody["status"] != "abandoned" {
		t.Fatalf("patches = %d, body = %#v", patches, patchBody)
	}
	if threadBody.Status != 1 || len(threadBody.Comments) != 1 ||
		threadBody.Comments[0].Content != "No longer needed" || threadBody.Comments[0].CommentType != 1 {
		t.Fatalf("thread body = %#v", threadBody)
	}
	if len(recorder.refs) != 2 || recorder.refs[0].Operation != "close" || recorder.refs[1].Operation != "comment" {
		t.Fatalf("mutations = %#v", recorder.refs)
	}
}

func TestADOProviderClosePullRequestReportsCompletedAsMerged(t *testing.T) {
	closedAt := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	var patches int
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
		}
		writeJSON(t, w, map[string]interface{}{
			"pullRequestId": 12,
			"status":        "completed",
			"closedDate":    closedAt.Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.ClosePullRequest(context.Background(), ClosePullRequestRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "12",
	})
	if err != nil {
		t.Fatalf("ClosePullRequest returned error: %v", err)
	}
	if result.Number != 12 || !result.Merged || result.State != "merged" || patches != 0 {
		t.Fatalf("result = %#v, patches = %d", result, patches)
	}
}

func TestADOProviderMergePullRequestCompletesWithLeaseAndStrategy(t *testing.T) {
	var patchBody map[string]interface{}
	recorder := &recordingRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{"pullRequestId": 12, "status": "active"})
		case http.MethodPatch:
			decodeJSON(t, r, &patchBody)
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId":   12,
				"status":          "completed",
				"lastMergeCommit": map[string]string{"commitId": "merge-sha"},
			})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider(
		"org",
		"project",
		"token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADOMutationRecorder(recorder),
	)
	result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository:      RepositoryRef{Name: "repo", Project: "project"},
		PullID:          "12",
		ExpectedHeadSHA: "head-sha",
		MergeMethod:     MergeMethodSquash,
	})
	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if result.Number != 12 || !result.Merged || result.MergeSHA != "merge-sha" {
		t.Fatalf("result = %#v", result)
	}
	if patchBody["status"] != "completed" {
		t.Fatalf("patch body = %#v", patchBody)
	}
	options, ok := patchBody["completionOptions"].(map[string]interface{})
	if !ok || options["mergeStrategy"] != "squash" || options["bypassPolicy"] != false {
		t.Fatalf("completion options = %#v", patchBody["completionOptions"])
	}
	lease, ok := patchBody["lastMergeSourceCommit"].(map[string]interface{})
	if !ok || lease["commitId"] != "head-sha" {
		t.Fatalf("lastMergeSourceCommit = %#v", patchBody["lastMergeSourceCommit"])
	}
	ref, ok := recorder.last()
	if !ok || ref.Operation != "merge" || ref.Ref != "org/project/repo#12" {
		t.Fatalf("mutation = %#v", ref)
	}
}

func TestADOProviderAutoCompleteLifecycle(t *testing.T) {
	var patchBody map[string]interface{}
	autoComplete := false
	completed := false
	recorder := &recordingRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/12", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			status := "active"
			if completed {
				status = "completed"
			}
			body := map[string]interface{}{
				"pullRequestId": 12,
				"status":        status,
				"createdBy":     map[string]string{"id": "creator-id"},
			}
			if autoComplete {
				body["autoCompleteSetBy"] = map[string]string{"id": "actor-id"}
			}
			if completed {
				body["lastMergeCommit"] = map[string]string{"commitId": "merge-sha"}
			}
			writeJSON(t, w, body)
		case http.MethodPatch:
			decodeJSON(t, r, &patchBody)
			autoComplete = true
			writeJSON(t, w, map[string]interface{}{
				"pullRequestId":     12,
				"status":            "active",
				"autoCompleteSetBy": map[string]string{"id": "actor-id"},
			})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		query := r.URL.Query()
		if query.Get("api-version") != "7.1-preview.1" ||
			query.Get("connectOptions") != "1" ||
			query.Get("lastChangeId") != "-1" ||
			query.Get("lastChangeId64") != "-1" {
			t.Fatalf("unexpected connection data query: %s", r.URL.RawQuery)
		}
		writeJSON(t, w, map[string]interface{}{
			"authenticatedUser": map[string]string{"id": "actor-id"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider(
		"org",
		"project",
		"token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADOMutationRecorder(recorder),
	)
	policy, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Branch:     "main",
	})
	if err != nil || policy.Policy != MergePolicyDirect {
		t.Fatalf("DetectMergePolicy = %#v, %v", policy, err)
	}
	enqueued, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{
		Repository:      RepositoryRef{Name: "repo", Project: "project"},
		PullID:          "12",
		ExpectedHeadSHA: "head-sha",
		MergeMethod:     MergeMethodRebase,
	})
	if err != nil {
		t.Fatalf("EnqueuePullRequest returned error: %v", err)
	}
	if enqueued.Number != 12 || enqueued.Merged {
		t.Fatalf("enqueue result = %#v", enqueued)
	}
	identity, ok := patchBody["autoCompleteSetBy"].(map[string]interface{})
	if !ok || identity["id"] != "actor-id" {
		t.Fatalf("autoCompleteSetBy = %#v", patchBody["autoCompleteSetBy"])
	}
	options, ok := patchBody["completionOptions"].(map[string]interface{})
	if !ok || options["mergeStrategy"] != "rebase" {
		t.Fatalf("completionOptions = %#v", patchBody["completionOptions"])
	}
	pending, err := provider.PollMergeQueueEntry(context.Background(), PollMergeQueueEntryRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "12",
	})
	if err != nil || pending.State != MergeQueueEntryPending || pending.QueueState != "auto-complete" {
		t.Fatalf("pending poll = %#v, %v", pending, err)
	}
	completed = true
	merged, err := provider.PollMergeQueueEntry(context.Background(), PollMergeQueueEntryRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		PullID:     "12",
	})
	if err != nil || merged.State != MergeQueueEntryMerged || merged.MergeSHA != "merge-sha" {
		t.Fatalf("merged poll = %#v, %v", merged, err)
	}
	ref, ok := recorder.last()
	if !ok || ref.Operation != "enqueue" {
		t.Fatalf("mutation = %#v", ref)
	}
}

func TestADOProviderPullRequestErrorsUseProviderErrorModel(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		transient bool
	}{
		{name: "auth", status: http.StatusUnauthorized},
		{name: "server", status: http.StatusServiceUnavailable, transient: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "provider failure", test.status)
			}))
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			_, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
				Repository: RepositoryRef{Name: "repo", Project: "project"},
				PullID:     "12",
			})
			if err == nil {
				t.Fatal("PollPullRequest returned nil error")
			}
			if !strings.Contains(err.Error(), "status "+strconv.Itoa(test.status)) {
				t.Fatalf("error = %q", err)
			}
			if got := IsTransientError(err); got != test.transient {
				t.Fatalf("IsTransientError(%v) = %t, want %t", err, got, test.transient)
			}
		})
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
				"pullRequestId":         12,
				"url":                   "api-pr-url",
				"status":                "active",
				"title":                 "Implement ADO reads",
				"createdBy":             map[string]string{"displayName": "Mona", "uniqueName": "mona@example.com"},
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
				"sourceRefName": "refs/heads/human/manual-fix",
				"targetRefName": "refs/heads/main",
			},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	prs, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Base:       "main",
		HeadPrefix: "goobers/",
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
	if got := pr.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"); got != "2026-07-15T20:30:00Z" {
		t.Fatalf("UpdatedAt = %q", got)
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
		{Path: "cmd/goobers/new.go", Status: "added"},
		{Path: "internal/runner/run.go", Status: "modified"},
		{Path: "old.txt", Status: "removed"},
		{Path: "new-name.txt", Status: "renamed"},
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
