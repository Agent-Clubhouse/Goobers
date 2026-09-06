package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGiteaProviderGetPullRequestMapsSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/pulls/12" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"number":           12,
			"title":            "Fix the thing",
			"html_url":         "https://gitea.test/acme/app/pulls/12",
			"state":            "open",
			"merge_commit_sha": "merge123",
			"head":             map[string]interface{}{"ref": "goobers/implementation/run-1", "sha": "head123"},
			"base":             map[string]interface{}{"ref": "main", "sha": "base123"},
		})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	pr, err := provider.GetPullRequest(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "12")
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if pr.Number != 12 || pr.Head != "goobers/implementation/run-1" || pr.HeadSHA != "head123" || pr.BaseSHA != "base123" || pr.MergeSHA != "merge123" {
		t.Fatalf("unexpected summary: %+v", pr)
	}
	// GetPullRequest never resolves check state.
	if pr.CheckState != "" {
		t.Fatalf("CheckState = %q, want empty", pr.CheckState)
	}
}

func TestGiteaProviderGetPullRequestRequiresPullID(t *testing.T) {
	provider := NewGiteaProvider("https://gitea.example.com", "token")
	if _, err := provider.GetPullRequest(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, ""); err == nil {
		t.Fatalf("expected error for empty pull id")
	}
}

func TestGiteaProviderListRecentlyClosedPullRequestsWindowsEarlyStopsAndFiltersHeadPrefix(t *testing.T) {
	updatedSince := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	var gotState, gotSort string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/pulls" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertMethod(t, r, http.MethodGet)
		gotState = r.URL.Query().Get("state")
		gotSort = r.URL.Query().Get("sort")
		writeJSON(t, w, []map[string]interface{}{
			// included: closed within window, matching head prefix
			{
				"number": 10, "html_url": "https://gitea.test/acme/app/pulls/10",
				"updated_at": "2026-07-20T00:00:00Z", "closed_at": "2026-07-20T00:00:00Z",
				"head": map[string]interface{}{"ref": "goobers/implementation/run-1", "sha": "aaa"},
				"base": map[string]interface{}{"ref": "main", "sha": "base"},
			},
			// excluded by head prefix (merged within window, wrong head)
			{
				"number": 11, "html_url": "https://gitea.test/acme/app/pulls/11",
				"updated_at": "2026-07-18T00:00:00Z", "merged_at": "2026-07-18T00:00:00Z",
				"head": map[string]interface{}{"ref": "someone/manual-fix", "sha": "bbb"},
				"base": map[string]interface{}{"ref": "main", "sha": "base"},
			},
			// excluded by window (updated in window but neither closed nor merged recently)
			{
				"number": 12, "html_url": "https://gitea.test/acme/app/pulls/12",
				"updated_at": "2026-07-16T00:00:00Z",
				"head":       map[string]interface{}{"ref": "goobers/implementation/run-2", "sha": "ccc"},
				"base":       map[string]interface{}{"ref": "main", "sha": "base"},
			},
			// early-stop boundary: updated before the window
			{
				"number": 13, "html_url": "https://gitea.test/acme/app/pulls/13",
				"updated_at": "2026-07-10T00:00:00Z", "closed_at": "2026-07-10T00:00:00Z",
				"head": map[string]interface{}{"ref": "goobers/implementation/run-3", "sha": "ddd"},
				"base": map[string]interface{}{"ref": "main", "sha": "base"},
			},
			// must NOT be reached — the scan stops at #13 despite this matching
			{
				"number": 14, "html_url": "https://gitea.test/acme/app/pulls/14",
				"updated_at": "2026-07-25T00:00:00Z", "closed_at": "2026-07-25T00:00:00Z",
				"head": map[string]interface{}{"ref": "goobers/implementation/run-4", "sha": "eee"},
				"base": map[string]interface{}{"ref": "main", "sha": "base"},
			},
		})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	out, err := provider.ListRecentlyClosedPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Base: "main", HeadPrefix: "goobers/",
	}, updatedSince)
	if err != nil {
		t.Fatalf("ListRecentlyClosedPullRequests: %v", err)
	}
	if gotState != "closed" || gotSort != "recentupdate" {
		t.Fatalf("query state=%q sort=%q, want closed/recentupdate", gotState, gotSort)
	}
	if len(out) != 1 || out[0].Number != 10 {
		t.Fatalf("out = %+v, want exactly PR #10", out)
	}
}

func TestGiteaProviderListRecentlyClosedPullRequestsRequiresUpdatedSince(t *testing.T) {
	provider := NewGiteaProvider("https://gitea.example.com", "token")
	if _, err := provider.ListRecentlyClosedPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
	}, time.Time{}); err == nil {
		t.Fatalf("expected error for zero updatedSince")
	}
}

func TestGiteaProviderCIFailuresReturnsFailingOnlyWithEmptyAnnotations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/commits/deadbeef/status" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"statuses": []map[string]interface{}{
			{"context": "build", "status": "failure", "target_url": "https://ci.test/build", "description": "compile error"},
			{"context": "lint", "status": "success"},
		}})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	failures, err := provider.CIFailures(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "deadbeef")
	if err != nil {
		t.Fatalf("CIFailures: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("len(failures) = %d, want 1 (only the failing status)", len(failures))
	}
	f := failures[0]
	if f.Name != "build" || f.State != CheckStateFailing || f.URL != "https://ci.test/build" || f.Summary != "compile error" {
		t.Fatalf("unexpected failure detail: %+v", f)
	}
	// Documented degradation: annotations are always the empty, non-nil slice.
	if f.Annotations == nil || len(f.Annotations) != 0 {
		t.Fatalf("Annotations = %#v, want empty non-nil slice", f.Annotations)
	}
}

func TestGiteaProviderCIFailuresRequiresRef(t *testing.T) {
	provider := NewGiteaProvider("https://gitea.example.com", "token")
	if _, err := provider.CIFailures(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, ""); err == nil {
		t.Fatalf("expected error for empty ref")
	}
}

func TestGiteaProviderBranchTipSHAReturnsCommitID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/branches/main" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"name": "main", "commit": map[string]interface{}{"id": "tip123"}})
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	sha, err := provider.BranchTipSHA(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "main")
	if err != nil {
		t.Fatalf("BranchTipSHA: %v", err)
	}
	if sha != "tip123" {
		t.Fatalf("sha = %q, want tip123", sha)
	}
}

func TestGiteaProviderBranchTipSHAMissingBranchIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	if _, err := provider.BranchTipSHA(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "gone"); err == nil {
		t.Fatalf("expected error for a missing branch (deleted base must not be swallowed)")
	}
}

func TestGiteaProviderUpdateCommentPatchesBodyAndRecordsMutation(t *testing.T) {
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/app/issues/comments/55" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		assertMethod(t, r, http.MethodPatch)
		decodeJSON(t, r, &gotBody)
		writeJSON(t, w, map[string]interface{}{
			"id": 55, "body": gotBody["body"],
			"html_url":  "https://gitea.test/acme/app/pulls/12#issuecomment-55",
			"issue_url": "https://gitea.test/api/v1/repos/acme/app/issues/12",
		})
	}))
	defer server.Close()

	recorder := &recordingRecorder{}
	provider := NewGiteaProvider(server.URL, "token", WithGiteaMutationRecorder(recorder))
	if err := provider.UpdateComment(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "55", "updated body"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	if gotBody["body"] != "updated body" {
		t.Fatalf("patched body = %q, want %q", gotBody["body"], "updated body")
	}
	ref, ok := recorder.last()
	if !ok {
		t.Fatal("UpdateComment recorded no external ref")
	}
	if ref.Provider != ProviderGitea || ref.Ref != "acme/app#12" ||
		ref.Operation != "comment" || ref.URL != "https://gitea.test/acme/app/pulls/12#issuecomment-55" {
		t.Fatalf("recorded ref = %+v, want gitea PR 12 comment mutation", ref)
	}
}

func TestGiteaProviderUpdateCommentRequiresCommentID(t *testing.T) {
	provider := NewGiteaProvider("https://gitea.example.com", "token")
	if err := provider.UpdateComment(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "", "body"); err == nil {
		t.Fatalf("expected error for empty comment id")
	}
}

func TestGiteaProviderListPullRequestReviewThreadsMapsReviewsAndInlineComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/12/reviews", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, []map[string]interface{}{
			{
				"id": 1, "state": "REQUEST_CHANGES", "body": "please fix",
				"commit_id": "abc", "submitted_at": "2026-07-20T00:00:00Z",
				"html_url": "https://gitea.test/acme/app/pulls/12#review-1",
				"user":     map[string]interface{}{"login": "reviewer"},
			},
		})
	})
	mux.HandleFunc("/api/v1/repos/acme/app/pulls/12/reviews/1/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, []map[string]interface{}{
			{
				"id": 100, "body": "bug here", "path": "main.go",
				"position": 5, "original_position": 4, "diff_hunk": "@@ -1 +1 @@",
				"created_at": "2026-07-20T00:00:00Z",
				"html_url":   "https://gitea.test/acme/app/pulls/12#comment-100",
				"user":       map[string]interface{}{"login": "reviewer"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	threads, err := provider.ListPullRequestReviewThreads(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "12")
	if err != nil {
		t.Fatalf("ListPullRequestReviewThreads: %v", err)
	}
	if len(threads.Reviews) != 1 {
		t.Fatalf("len(Reviews) = %d, want 1", len(threads.Reviews))
	}
	review := threads.Reviews[0]
	if review.Author != "reviewer" || review.State != "REQUEST_CHANGES" || review.Body != "please fix" || review.CommitSHA != "abc" {
		t.Fatalf("unexpected review: %+v", review)
	}
	if len(threads.InlineComments) != 1 {
		t.Fatalf("len(InlineComments) = %d, want 1", len(threads.InlineComments))
	}
	c := threads.InlineComments[0]
	if c.Author != "reviewer" || c.Body != "bug here" || c.Path != "main.go" {
		t.Fatalf("unexpected inline comment: %+v", c)
	}
	// Best-effort line mapping from diff position/original_position.
	if c.Line != 5 || c.OriginalLine != 4 {
		t.Fatalf("Line/OriginalLine = %d/%d, want 5/4", c.Line, c.OriginalLine)
	}
	// Documented degradation: Gitea exposes no thread resolution/outdatedness.
	if c.IsResolved || c.IsOutdated {
		t.Fatalf("IsResolved=%v IsOutdated=%v, want both false", c.IsResolved, c.IsOutdated)
	}
}

func TestGiteaProviderListPullRequestReviewThreadsRejectsNonPositivePullID(t *testing.T) {
	provider := NewGiteaProvider("https://gitea.example.com", "token")
	if _, err := provider.ListPullRequestReviewThreads(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "0"); err == nil {
		t.Fatalf("expected error for non-positive pull id")
	}
}
