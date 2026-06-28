package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestADOProviderMapsWorkItemsAndStatus(t *testing.T) {
	mux := http.NewServeMux()
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

func TestADOProviderRepoAndBacklogOperations(t *testing.T) {
	var patchBody []adoPatchOperation
	var reviewerPath string
	mux := http.NewServeMux()
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
		assertMethod(t, r, http.MethodPost)
		writeJSON(t, w, map[string]interface{}{"pullRequestId": 12, "url": "pr-url"})
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
	if len(patchBody) == 0 || patchBody[0].Path != "/fields/System.Tags" || patchBody[0].Value != "route/backend; goobers/status:in-progress" {
		t.Fatalf("patch body = %#v", patchBody)
	}
}

func TestADOProviderCreateWorkItemSubscribeAndClone(t *testing.T) {
	var wiqlCalls int
	mux := http.NewServeMux()
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

	runner := &fakeRunner{}
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
}
