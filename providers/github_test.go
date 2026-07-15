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
	// issueLabels is the live label set for issue 7; the label sub-API handlers
	// below mutate it so the re-GET in UpdateWorkItemStatus observes the swap
	// (status updates now go through add/remove-label, not a whole-set PATCH — #140).
	issueLabels := []string{"route/backend", "goobers/status:claimed"}
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
		switch r.Method {
		case http.MethodGet:
			// OpenPullRequest's idempotency check (#132): no existing open PR.
			writeJSON(t, w, []map[string]interface{}{})
		case http.MethodPost:
			writeJSON(t, w, map[string]interface{}{"id": 44, "number": 9, "html_url": "pr-url"})
		default:
			t.Fatalf("unexpected pulls method %s", r.Method)
		}
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
	labelObjs := func() []map[string]string {
		out := make([]map[string]string, 0, len(issueLabels))
		for _, l := range issueLabels {
			out = append(out, map[string]string{"name": l})
		}
		return out
	}
	issueBody := func() map[string]interface{} {
		return map[string]interface{}{
			"id": 123, "number": 7, "title": "Fix API", "state": "open",
			"labels": labelObjs(),
		}
	}
	// Longer-prefix label routes must be registered before /issues/7 so the mux
	// dispatches them; ServeMux matches the longest pattern.
	mux.HandleFunc("/repos/acme/app/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var body struct {
			Labels []string `json:"labels"`
		}
		decodeJSON(t, r, &body)
		issueLabels = append(issueLabels, body.Labels...)
		writeJSON(t, w, labelObjs())
	})
	mux.HandleFunc("/repos/acme/app/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodDelete)
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/app/issues/7/labels/")
		var kept []string
		for _, l := range issueLabels {
			if l != name {
				kept = append(kept, l)
			}
		}
		issueLabels = kept
		writeJSON(t, w, labelObjs())
	})
	mux.HandleFunc("/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, issueBody())
		case http.MethodPatch:
			// A status update must swap labels via the sub-API, never PATCH the
			// whole label set (that read-modify-write clobbers concurrent edits).
			var body map[string]interface{}
			decodeJSON(t, r, &body)
			if _, ok := body["labels"]; ok {
				t.Fatalf("status update must not PATCH labels; got %#v", body)
			}
			writeJSON(t, w, issueBody())
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
	// The non-status label must survive the swap, and only the status label may
	// change: claimed → in-progress, route/backend untouched (#140).
	if strings.Join(issueLabels, ",") != "route/backend,goobers/status:in-progress" {
		t.Fatalf("labels after status update = %#v; want route/backend preserved and status swapped to in-progress", issueLabels)
	}
	if len(item.Labels) != 2 {
		t.Fatalf("returned item labels = %#v", item.Labels)
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
		if r.Method == http.MethodGet {
			// OpenPullRequest's idempotency check (#132): no existing open PR.
			writeJSON(t, w, []map[string]interface{}{})
			return
		}
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
		if r.Method == http.MethodGet {
			// OpenPullRequest's idempotency check (#132): no existing open PR.
			writeJSON(t, w, []map[string]interface{}{})
			return
		}
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

// TestGitHubProviderOpenPullRequestIsIdempotentOnRepass proves #132's fix: a
// second OpenPullRequest call for the same stable run branch (a workflow
// repass through open-pr) finds the already-open PR via the head/base lookup
// and PATCHes it instead of POSTing a duplicate (which GitHub would 422 on).
func TestGitHubProviderOpenPullRequestIsIdempotentOnRepass(t *testing.T) {
	var posts, patches int
	var patchedTitle string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if got := r.URL.Query().Get("head"); got != "acme:goobers/impl/run-1" {
				t.Fatalf("lookup head query = %q", got)
			}
			if got := r.URL.Query().Get("base"); got != "main" {
				t.Fatalf("lookup base query = %q", got)
			}
			if got := r.URL.Query().Get("state"); got != "open" {
				t.Fatalf("lookup state query = %q", got)
			}
			writeJSON(t, w, []map[string]interface{}{
				{"number": 9, "html_url": "https://github.com/acme/app/pull/9"},
			})
		case http.MethodPost:
			posts++
			writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "https://github.com/acme/app/pull/9"})
		default:
			t.Fatalf("unexpected method %s on /pulls", r.Method)
		}
	})
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		var body map[string]interface{}
		decodeJSON(t, r, &body)
		patchedTitle, _ = body["title"].(string)
		patches++
		writeJSON(t, w, map[string]interface{}{"number": 9, "html_url": "https://github.com/acme/app/pull/9"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.OpenPullRequest(context.Background(), PullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Title:      "Implement #13 (repass)", Body: "Adds PR polling.",
		Head: "goobers/impl/run-1", Base: "main", RunID: "run-1",
	})
	if err != nil {
		t.Fatalf("OpenPullRequest returned error: %v", err)
	}
	if result.Number != 9 {
		t.Fatalf("result.Number = %d, want 9 (the existing PR)", result.Number)
	}
	if posts != 0 {
		t.Fatalf("expected no POST (duplicate-create) call, got %d", posts)
	}
	if patches != 1 {
		t.Fatalf("expected exactly one PATCH (update) call, got %d", patches)
	}
	if patchedTitle != "Implement #13 (repass)" {
		t.Fatalf("patched title = %q", patchedTitle)
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

// TestGitHubProviderListPullRequestsFiltersByHeadPrefixAndReportsCheckState
// is issue #359's selection-stage acceptance: the pulls-list endpoint has no
// server-side head-prefix filter, so the provider must apply it client-side,
// and each returned candidate carries its own combined check state (queried
// per-PR, same mechanism PollPullRequest already uses).
func TestGitHubProviderListPullRequestsFiltersByHeadPrefixAndReportsCheckState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		if got := r.URL.Query().Get("base"); got != "main" {
			t.Fatalf("base query = %q, want main", got)
		}
		writeJSON(t, w, []map[string]interface{}{
			{
				"number": 10, "html_url": "https://github.com/acme/app/pull/10", "draft": false,
				"updated_at": "2026-07-15T00:00:00Z",
				"head":       map[string]interface{}{"ref": "goobers/implementation/run-1", "sha": "aaa111"},
				"base":       map[string]interface{}{"ref": "main", "sha": "base111"},
				"labels":     []map[string]string{{"name": "goobers:needs-remediation"}},
			},
			{
				// A human-authored PR (no goobers/ prefix) must be excluded.
				"number": 11, "html_url": "https://github.com/acme/app/pull/11", "draft": false,
				"updated_at": "2026-07-15T00:00:00Z",
				"head":       map[string]interface{}{"ref": "someone/manual-fix", "sha": "bbb222"},
				"base":       map[string]interface{}{"ref": "main", "sha": "base111"},
			},
		})
	})
	mux.HandleFunc("/repos/acme/app/commits/aaa111/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"state": "success", "statuses": []map[string]interface{}{
			{"context": "ci", "state": "success"},
		}})
	})
	mux.HandleFunc("/repos/acme/app/commits/aaa111/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"check_runs": []map[string]interface{}{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	out, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Base: "main", HeadPrefix: "goobers/",
	})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1 (the human-authored PR must be excluded)", len(out))
	}
	pr := out[0]
	if pr.Number != 10 || pr.Head != "goobers/implementation/run-1" || pr.Base != "main" ||
		pr.HeadSHA != "aaa111" || pr.BaseSHA != "base111" || pr.Draft {
		t.Fatalf("unexpected summary: %+v", pr)
	}
	if len(pr.Labels) != 1 || pr.Labels[0] != "goobers:needs-remediation" {
		t.Fatalf("Labels = %v, want [goobers:needs-remediation]", pr.Labels)
	}
	if pr.CheckState != CheckStatePassing {
		t.Fatalf("CheckState = %q, want passing", pr.CheckState)
	}
}

// TestGitHubProviderPullRequestFilesListsTouchedFiles is issue #359's
// sibling-set context gathering: given another open PR's number, list the
// files it touches for cross-PR conflict/drift detection.
func TestGitHubProviderPullRequestFilesListsTouchedFiles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/12/files", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, []map[string]interface{}{
			{"filename": "internal/runner/run.go", "status": "modified", "additions": 12, "deletions": 3},
			{"filename": "cmd/goobers/new.go", "status": "added", "additions": 40, "deletions": 0},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	files, err := provider.PullRequestFiles(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "12")
	if err != nil {
		t.Fatalf("PullRequestFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	if files[0].Path != "internal/runner/run.go" || files[0].Status != "modified" || files[0].Additions != 12 || files[0].Deletions != 3 {
		t.Fatalf("unexpected file[0]: %+v", files[0])
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
		if r.Method == http.MethodGet {
			// OpenPullRequest's idempotency check (#132): no existing open PR.
			writeJSON(t, w, []map[string]interface{}{})
			return
		}
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

func TestGitHubProviderMergePullRequestSucceeds(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody map[string]interface{}
	mux.HandleFunc("/repos/acme/app/pulls/9/merge", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPut)
		decodeJSON(t, r, &gotBody)
		writeJSON(t, w, map[string]interface{}{"sha": "abc123", "merged": true, "message": "Pull Request successfully merged"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(rec))
	result, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
		ExpectedHeadSHA: "deadbeef", CommitMessage: "merged by merge-review",
	})
	if err != nil {
		t.Fatalf("MergePullRequest returned error: %v", err)
	}
	if !result.Merged || result.MergeSHA != "abc123" || result.Number != 9 {
		t.Fatalf("result = %#v", result)
	}
	if gotBody["sha"] != "deadbeef" {
		t.Fatalf("request body sha = %v, want deadbeef (the SHA-pin optimistic-concurrency guard)", gotBody["sha"])
	}
	if gotBody["commit_message"] != "merged by merge-review" {
		t.Fatalf("request body commit_message = %v", gotBody["commit_message"])
	}
	ref, ok := rec.last()
	if !ok {
		t.Fatalf("expected a recorded external ref")
	}
	if ref.Operation != "merge" {
		t.Fatalf("Operation = %q, want merge", ref.Operation)
	}
}

// TestGitHubProviderMergePullRequestRefusedOnSHAMismatch proves a stale
// SHA-pin is refused server-side (GitHub's own optimistic-concurrency guard,
// belt-and-suspenders alongside the caller's own D6 re-check) — surfaced as
// an error, never a silent Merged=false.
func TestGitHubProviderMergePullRequestRefusedOnSHAMismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/9/merge", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Head branch was modified"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	_, err := provider.MergePullRequest(context.Background(), MergePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "stale-sha",
	})
	if err == nil {
		t.Fatal("expected an error for a stale SHA-pin (409), got nil")
	}
}

// TestGitHubProviderPollPullRequestSurfacesDraftAndSHAs is #360's regression:
// a conjunctive auto-merge action re-checks not-draft and the SHA-pin (D6)
// against PollPullRequest's live result — these fields must actually reach
// the caller, not just exist unpopulated on the struct.
func TestGitHubProviderPollPullRequestSurfacesDraftAndSHAs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"number": 9, "state": "open", "draft": true, "html_url": "https://github.com/acme/app/pull/9",
			"head": map[string]interface{}{"sha": "headsha123"},
			"base": map[string]interface{}{"sha": "basesha456"},
		})
	})
	mux.HandleFunc("/repos/acme/app/pulls/9/reviews", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{})
	})
	mux.HandleFunc("/repos/acme/app/commits/headsha123/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"state": "success", "statuses": []map[string]interface{}{}})
	})
	mux.HandleFunc("/repos/acme/app/commits/headsha123/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"check_runs": []map[string]interface{}{}})
	})
	mux.HandleFunc("/repos/acme/app/issues/9/comments", func(w http.ResponseWriter, r *http.Request) {
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
	if !result.Draft {
		t.Fatal("Draft = false, want true")
	}
	if result.HeadSHA != "headsha123" {
		t.Fatalf("HeadSHA = %q, want headsha123", result.HeadSHA)
	}
	if result.BaseSHA != "basesha456" {
		t.Fatalf("BaseSHA = %q, want basesha456", result.BaseSHA)
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
