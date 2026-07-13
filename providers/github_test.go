package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitHubProviderMapsWorkItemsAndStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if got := r.URL.Query().Get("labels"); got != "route/backend" {
			t.Fatalf("labels query = %q", got)
		}
		writeJSON(t, w, []map[string]interface{}{
			{
				"id":       123,
				"number":   7,
				"title":    "Fix API",
				"body":     "Make it pass",
				"state":    "open",
				"html_url": "https://github.com/acme/app/issues/7",
				"labels": []map[string]string{
					{"name": "route/backend"},
					{"name": "goobers/status:claimed"},
				},
				"assignees": []map[string]string{{"login": "mona"}},
				"milestone": map[string]interface{}{
					"number":   2,
					"title":    "v1",
					"html_url": "https://github.com/acme/app/milestone/2",
				},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Labels:     []string{"route/backend"},
	})
	if err != nil {
		t.Fatalf("ListWorkItems returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d", len(items))
	}
	item := items[0]
	if item.Provider != ProviderGitHub || item.ID != "7" || item.Status != WorkItemStatusClaimed {
		t.Fatalf("unexpected item mapping: %#v", item)
	}
	if !item.HasLabel("route/backend") {
		t.Fatalf("expected scheduler routing label to be preserved: %#v", item.Labels)
	}
	if item.Parent == nil || item.Parent.Type != "milestone" {
		t.Fatalf("expected hierarchy parent to be preserved: %#v", item.Parent)
	}
}

func TestGitHubProviderRepoAndBacklogOperations(t *testing.T) {
	var patchedLabels []string
	var requestedReviewers []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"ref": "refs/heads/main", "object": map[string]string{"sha": "base-sha"}})
	})
	mux.HandleFunc("/repos/acme/app/git/refs", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		writeJSON(t, w, map[string]interface{}{"ref": "refs/heads/work", "url": "ref-url", "object": map[string]string{"sha": "base-sha"}})
	})
	mux.HandleFunc("/repos/acme/app/contents/README.md", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]string{"sha": "existing-sha"})
		case http.MethodPut:
			var body map[string]string
			decodeJSON(t, r, &body)
			if body["sha"] != "existing-sha" {
				t.Fatalf("expected existing file sha in commit body, got %#v", body)
			}
			writeJSON(t, w, map[string]interface{}{"commit": map[string]string{"sha": "commit-sha", "html_url": "commit-url"}})
		default:
			t.Fatalf("unexpected contents method %s", r.Method)
		}
	})
	mux.HandleFunc("/repos/acme/app/contents/old.txt", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]string{"sha": "old-sha"})
		case http.MethodDelete:
			var body map[string]string
			decodeJSON(t, r, &body)
			if body["sha"] != "old-sha" {
				t.Fatalf("expected delete sha in commit body, got %#v", body)
			}
			writeJSON(t, w, map[string]interface{}{"commit": map[string]string{"sha": "delete-sha", "html_url": "delete-url"}})
		default:
			t.Fatalf("unexpected old contents method %s", r.Method)
		}
	})
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		writeJSON(t, w, map[string]interface{}{"id": 44, "number": 9, "html_url": "pr-url"})
	})
	mux.HandleFunc("/repos/acme/app/pulls/9/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body struct {
			Reviewers []string `json:"reviewers"`
		}

		decodeJSON(t, r, &body)
		requestedReviewers = body.Reviewers
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 123, "number": 7, "title": "Fix API", "state": "open",
				"labels": []map[string]string{{"name": "route/backend"}, {"name": "goobers/status:claimed"}},
			})
		case http.MethodPatch:
			var body struct {
				Labels []string `json:"labels"`
			}
			decodeJSON(t, r, &body)
			patchedLabels = body.Labels
			writeJSON(t, w, map[string]interface{}{
				"id": 123, "number": 7, "title": "Fix API", "state": "open",
				"labels": []map[string]string{{"name": "route/backend"}, {"name": "goobers/status:in-progress"}},
			})
		default:
			t.Fatalf("unexpected issue method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	if branch, err := provider.CreateBranch(context.Background(), BranchRequest{Repository: repo, BaseBranch: "main", Name: "work"}); err != nil || branch.Name != "work" {
		t.Fatalf("CreateBranch = %#v, %v", branch, err)
	}
	files := []CommitFile{
		{Path: "README.md", Content: "hello"},
		{Path: "old.txt", ChangeType: string(CommitChangeDelete)},
	}
	if commit, err := provider.Commit(context.Background(), CommitRequest{Repository: repo, Branch: "work", Message: "docs", Files: files}); err != nil || commit.SHA != "delete-sha" {
		t.Fatalf("Commit = %#v, %v", commit, err)
	}
	pr, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{Repository: repo, Title: "Fix", Head: "work", Base: "main"})
	if err != nil || pr.Number != 9 {
		t.Fatalf("OpenPullRequest = %#v, %v", pr, err)
	}
	if pr.ID != "9" {
		t.Fatalf("OpenPullRequest ID = %q, want PR number", pr.ID)
	}
	if err := provider.RequestReview(context.Background(), ReviewRequest{Repository: repo, PullID: pr.ID, Reviewers: []string{"qa-1"}}); err != nil {
		t.Fatalf("RequestReview returned error: %v", err)
	}
	if strings.Join(requestedReviewers, ",") != "qa-1" {
		t.Fatalf("requested reviewers = %#v", requestedReviewers)
	}
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{Repository: repo, ID: "7", Status: WorkItemStatusInProgress})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus returned error: %v", err)
	}
	if item.Status != WorkItemStatusInProgress {
		t.Fatalf("updated item status = %q", item.Status)
	}
	if strings.Join(patchedLabels, ",") != "route/backend,goobers/status:in-progress" {
		t.Fatalf("patched labels = %#v", patchedLabels)
	}
}

func TestGitHubProviderCreateWorkItemAndClone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body struct {
			Title  string   `json:"title"`
			Labels []string `json:"labels"`
		}
		decodeJSON(t, r, &body)
		if body.Title != "New work" || strings.Join(body.Labels, ",") != "route/backend,goobers/status:claimed" {
			t.Fatalf("unexpected create body: %#v", body)
		}
		writeJSON(t, w, map[string]interface{}{
			"id": 999, "number": 11, "title": body.Title, "state": "open",
			"labels": []map[string]string{{"name": "route/backend"}, {"name": "goobers/status:claimed"}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	runner := &fakeRunner{}
	provider := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.BaseURL = server.URL
		p.Runner = runner
	})
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	item, err := provider.CreateWorkItem(context.Background(), CreateWorkItemRequest{
		Repository: repo,
		Title:      "New work",
		Labels:     []string{"route/backend"},
		Status:     WorkItemStatusClaimed,
	})
	if err != nil || item.ID != "11" || item.Status != WorkItemStatusClaimed {
		t.Fatalf("CreateWorkItem = %#v, %v", item, err)
	}
	if provider.Kind() != ProviderGitHub {
		t.Fatalf("Kind = %q", provider.Kind())
	}
	clone, err := provider.CloneRepository(context.Background(), CloneRequest{Repository: repo, Destination: "/tmp/app", Branch: "main"})
	if err != nil {
		t.Fatalf("CloneRepository returned error: %v", err)
	}
	if clone.Path != "/tmp/app" || !strings.Contains(strings.Join(runner.args, " "), "clone") {
		t.Fatalf("unexpected clone result=%#v args=%#v", clone, runner.args)
	}
}

func TestGitHubProviderErrorPaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	if _, err := provider.GetWorkItem(context.Background(), repo, "7"); err == nil {
		t.Fatal("expected non-2xx response to return an error")
	}
	if _, err := provider.CreateBranch(context.Background(), BranchRequest{Repository: repo}); err == nil {
		t.Fatal("expected missing branch name to return an error")
	}
	if _, err := provider.Subscribe(context.Background(), TriggerSubscription{Kind: TriggerWebhook, Repository: repo}); err == nil {
		t.Fatal("expected unsupported webhook subscription to return an error")
	}
}

func TestGitHubProviderPollingSubscriptionContinues(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		calls++
		number := calls
		writeJSON(t, w, []map[string]interface{}{
			{
				"id": number, "number": number, "title": "Work", "state": "open",
				"labels": []map[string]string{{"name": "route/backend"}},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := provider.Subscribe(ctx, TriggerSubscription{
		Kind:         TriggerPolling,
		Repository:   RepositoryRef{Owner: "acme", Name: "app"},
		PollInterval: 1,
	})
	if err != nil {
		t.Fatalf("Subscribe returned error: %v", err)
	}
	first := <-events
	second := <-events
	if first.Item.ID == second.Item.ID {
		t.Fatalf("expected polling subscription to continue and emit changed items, got %q twice", first.Item.ID)
	}
}

func TestGitHubProviderOpenPullRequestStampsRunIDFooter(t *testing.T) {
	var gotBody map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		decodeJSON(t, r, &gotBody)
		writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "https://github.com/acme/app/pull/9"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	_, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "Implement #13", Body: "Adds PR polling.", Head: "goobers/impl/run-1", Base: "main",
		RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("OpenPullRequest returned error: %v", err)
	}
	body, _ := gotBody["body"].(string)
	if !strings.Contains(body, "Adds PR polling.") || !strings.Contains(body, "goobers run-id: run-1") {
		t.Fatalf("body missing run-id footer: %q", body)
	}
}

func TestGitHubProviderOpenPullRequestFooterNoOpWithoutRunID(t *testing.T) {
	var gotBody map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		decodeJSON(t, r, &gotBody)
		writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "https://github.com/acme/app/pull/9"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	_, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "Implement #13", Body: "Adds PR polling.", Head: "goobers/impl/run-1", Base: "main",
	})
	if err != nil {
		t.Fatalf("OpenPullRequest returned error: %v", err)
	}
	if body, _ := gotBody["body"].(string); body != "Adds PR polling." {
		t.Fatalf("body = %q, want unchanged (no run-id)", body)
	}
}

func TestGitHubProviderPollPullRequestAggregatesState(t *testing.T) {
	mux := http.NewServeMux()
	mergeable := true
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"number": 9, "state": "open", "merged": false, "mergeable": mergeable,
			"html_url": "https://github.com/acme/app/pull/9",
			"head":     map[string]interface{}{"sha": "deadbeef"},
		})
	})
	mux.HandleFunc("/repos/acme/app/pulls/9/reviews", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"state": "CHANGES_REQUESTED", "user": map[string]string{"login": "alice"}},
			{"state": "COMMENTED", "user": map[string]string{"login": "bob"}},
			{"state": "APPROVED", "user": map[string]string{"login": "alice"}},
		})
	})
	mux.HandleFunc("/repos/acme/app/commits/deadbeef/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"state": "failure",
			"statuses": []map[string]interface{}{
				{"context": "legacy-ci", "state": "failure", "target_url": "https://ci/legacy", "description": "boom"},
			},
		})
	})
	mux.HandleFunc("/repos/acme/app/commits/deadbeef/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"check_runs": []map[string]interface{}{
				{"name": "unit-tests", "status": "completed", "conclusion": "success", "html_url": "https://ci/unit"},
				{"name": "e2e", "status": "in_progress", "html_url": "https://ci/e2e"},
			},
		})
	})
	mux.HandleFunc("/repos/acme/app/issues/9/comments", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got == "" {
			t.Fatalf("expected since query param")
		}
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "body": "fix this", "html_url": "https://github.com/acme/app/pull/9#comment-1", "user": map[string]string{"login": "carol"}, "created_at": "2026-07-13T00:00:00Z"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	since := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	result, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", CommentsSince: &since,
	})
	if err != nil {
		t.Fatalf("PollPullRequest returned error: %v", err)
	}
	if result.ReviewDecision != ReviewDecisionApproved {
		t.Fatalf("ReviewDecision = %q, want approved (alice's later APPROVED supersedes her own CHANGES_REQUESTED)", result.ReviewDecision)
	}
	if result.RequestedChanges != 0 {
		t.Fatalf("RequestedChanges = %d, want 0", result.RequestedChanges)
	}
	if result.CheckState != CheckStateFailing {
		t.Fatalf("CheckState = %q, want failing (legacy status reports failure)", result.CheckState)
	}
	if len(result.Checks) != 3 {
		t.Fatalf("len(Checks) = %d, want 3 (1 status + 2 check-runs)", len(result.Checks))
	}
	if result.Mergeable == nil || !*result.Mergeable {
		t.Fatalf("Mergeable = %v, want true", result.Mergeable)
	}
	if len(result.CommentsSince) != 1 || result.CommentsSince[0].Author != "carol" {
		t.Fatalf("CommentsSince = %#v", result.CommentsSince)
	}
}

func TestGitHubProviderPollPullRequestChangesRequestedWins(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"number": 9, "state": "open", "html_url": "https://github.com/acme/app/pull/9"})
	})
	mux.HandleFunc("/repos/acme/app/pulls/9/reviews", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"state": "APPROVED", "user": map[string]string{"login": "alice"}},
			{"state": "CHANGES_REQUESTED", "user": map[string]string{"login": "bob"}},
		})
	})
	mux.HandleFunc("/repos/acme/app/issues/9/comments", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "" {
			t.Fatalf("since query param = %q, want none (CommentsSince not set)", got)
		}
		writeJSON(t, w, []map[string]interface{}{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.PollPullRequest(context.Background(), PullRequestPollRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
	})
	if err != nil {
		t.Fatalf("PollPullRequest returned error: %v", err)
	}
	if result.ReviewDecision != ReviewDecisionChangesRequested {
		t.Fatalf("ReviewDecision = %q, want changes_requested (outstanding request beats another reviewer's approval)", result.ReviewDecision)
	}
	if result.RequestedChanges != 1 {
		t.Fatalf("RequestedChanges = %d, want 1", result.RequestedChanges)
	}
	if result.CheckState != CheckStatePending {
		t.Fatalf("CheckState = %q, want pending (no head sha, no checks polled)", result.CheckState)
	}
}

func TestGitHubProviderClosePullRequestDetectsMergedVsClosed(t *testing.T) {
	mux := http.NewServeMux()
	var gotComment map[string]string
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		var body map[string]string
		decodeJSON(t, r, &body)
		if body["state"] != "closed" {
			t.Fatalf("state = %q, want closed", body["state"])
		}
		writeJSON(t, w, map[string]interface{}{"number": 9, "state": "closed", "merged": true})
	})
	mux.HandleFunc("/repos/acme/app/issues/9/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		decodeJSON(t, r, &gotComment)
		writeJSON(t, w, map[string]interface{}{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.ClosePullRequest(context.Background(), ClosePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", Comment: "landed, thanks!",
	})
	if err != nil {
		t.Fatalf("ClosePullRequest returned error: %v", err)
	}
	if !result.Merged || result.State != "merged" {
		t.Fatalf("result = %#v, want merged=true state=merged", result)
	}
	if gotComment["body"] != "landed, thanks!" {
		t.Fatalf("comment body = %q", gotComment["body"])
	}
}

func TestGitHubProviderClosePullRequestUnmerged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"number": 9, "state": "closed", "merged": false})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.ClosePullRequest(context.Background(), ClosePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
	})
	if err != nil {
		t.Fatalf("ClosePullRequest returned error: %v", err)
	}
	if result.Merged || result.State != "closed" {
		t.Fatalf("result = %#v, want merged=false state=closed", result)
	}
}

func TestGitHubProviderOpenPullRequestRecordsMutation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "https://github.com/acme/app/pull/9"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(rec))
	_, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "Implement #13", Body: "Adds PR polling.", Head: "goobers/impl/run-1", Base: "main", RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("OpenPullRequest returned error: %v", err)
	}
	ref, ok := rec.last()
	if !ok {
		t.Fatalf("expected a recorded external ref")
	}
	if ref.Ref != "acme/app#9" || ref.Operation != "open" || ref.RunID != "run-1" {
		t.Fatalf("ref = %#v", ref)
	}
	if _, ok := ref.Fields["body"]; !ok {
		t.Fatalf("expected body field digest, got %#v", ref.Fields)
	}
}

func TestGitHubProviderClosePullRequestRecordsMergeVsClose(t *testing.T) {
	for _, tc := range []struct {
		name      string
		merged    bool
		operation string
	}{
		{"merged", true, "merge"},
		{"closed", false, "close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, map[string]interface{}{"number": 9, "state": "closed", "merged": tc.merged, "html_url": "https://github.com/acme/app/pull/9"})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			rec := &recordingRecorder{}
			provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(rec))
			_, err := provider.ClosePullRequest(context.Background(), ClosePullRequestRequest{
				Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
			})
			if err != nil {
				t.Fatalf("ClosePullRequest returned error: %v", err)
			}
			ref, ok := rec.last()
			if !ok {
				t.Fatalf("expected a recorded external ref")
			}
			if ref.Operation != tc.operation {
				t.Fatalf("Operation = %q, want %q", ref.Operation, tc.operation)
			}
		})
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func decodeJSON(t *testing.T, r *http.Request, out interface{}) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatalf("decode request: %v", err)
	}
}

func assertMethod(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if r.Method != want {
		t.Fatalf("method = %s, want %s", r.Method, want)
	}
}

type fakeRunner struct {
	name string
	args []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.name = name
	f.args = append([]string(nil), args...)
	return nil, nil
}
