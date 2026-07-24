package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubProviderUpdateBranchUsesExpectedHeadLease(t *testing.T) {
	var requestBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPut)
		if r.URL.Path != "/repos/acme/app/pulls/42/update-branch" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		decodeJSON(t, r, &requestBody)
		w.WriteHeader(http.StatusAccepted)
		writeJSON(t, w, map[string]string{
			"message": "Updating pull request branch.",
			"url":     "https://api.github.test/repos/acme/app/pulls/42",
		})
	}))
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(rec))
	result, err := provider.UpdateBranch(context.Background(), UpdateBranchRequest{
		Repository:      RepositoryRef{Owner: "acme", Name: "app"},
		PullID:          "42",
		ExpectedHeadSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("UpdateBranch returned error: %v", err)
	}
	if requestBody["expected_head_sha"] != "deadbeef" {
		t.Fatalf("expected_head_sha = %q, want deadbeef", requestBody["expected_head_sha"])
	}
	if result.Number != 42 || result.Message != "Updating pull request branch." {
		t.Fatalf("result = %+v", result)
	}
	ref, ok := rec.last()
	if !ok || ref.Operation != "update-branch" {
		t.Fatalf("recorded ref = (%+v, %v), want update-branch", ref, ok)
	}
}

func TestGitHubProviderUpdateBranchReturnsTypedRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(t, w, map[string]string{"message": "expected head SHA did not match"})
	}))
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	_, err := provider.UpdateBranch(context.Background(), UpdateBranchRequest{
		Repository:      RepositoryRef{Owner: "acme", Name: "app"},
		PullID:          "42",
		ExpectedHeadSHA: "stale",
	})
	var updateErr *UpdateBranchError
	if !errors.As(err, &updateErr) {
		t.Fatalf("error = %v, want *UpdateBranchError", err)
	}
	if updateErr.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(updateErr.Message, "expected head SHA") {
		t.Fatalf("typed error = %+v", updateErr)
	}
}

func TestGitHubProviderUpdateBranchRequiresExpectedHead(t *testing.T) {
	provider := NewGitHubProvider("token")
	_, err := provider.UpdateBranch(context.Background(), UpdateBranchRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		PullID:     "42",
	})
	if err == nil || !strings.Contains(err.Error(), "expected head SHA is required") {
		t.Fatalf("error = %v, want expected-head validation", err)
	}
}

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

func TestGitHubProviderDeleteBranch(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantDeleted bool
		wantErr     bool
	}{
		{name: "deleted", status: http.StatusNoContent, wantDeleted: true},
		{name: "already absent", status: http.StatusNotFound},
		{name: "provider failure", status: http.StatusForbidden, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodDelete {
					t.Fatalf("method = %s, want DELETE", r.Method)
				}
				if r.URL.Path != "/repos/acme/app/git/refs/heads/goobers/implementation/run-1" {
					t.Fatalf("path = %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			provider := NewGitHubProvider("token", func(p *GitHubProvider) {
				p.BaseURL = server.URL
				p.maxRetries = 0
			})
			result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
				Repository: RepositoryRef{Owner: "acme", Name: "app"},
				Name:       "goobers/implementation/run-1",
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("DeleteBranch error = %v, wantErr %t", err, tc.wantErr)
			}
			if result.Deleted != tc.wantDeleted {
				t.Fatalf("Deleted = %t, want %t", result.Deleted, tc.wantDeleted)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1", calls)
			}
		})
	}
}

func TestGitHubProviderDeleteBranchUsesExpectedSHALease(t *testing.T) {
	runner := &fakeEnvironmentRunner{}
	provider := NewGitHubProvider("secret-token", func(p *GitHubProvider) {
		p.Runner = runner
	})
	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
		Repository: RepositoryRef{
			Owner: "acme",
			Name:  "app",
			URL:   "https://github.example/acme/app.git",
		},
		Name:        "goobers/implementation/run-1",
		ExpectedSHA: "validated-sha",
	})
	if err != nil {
		t.Fatalf("DeleteBranch returned error: %v", err)
	}
	if !result.Deleted {
		t.Fatal("Deleted = false, want true")
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "git" ||
		!slicesEqual(runner.calls[0].args[:3], []string{"init", "--bare", "--quiet"}) {
		t.Fatalf("runner calls = %+v", runner.calls)
	}
	if len(runner.envCalls) != 1 {
		t.Fatalf("environment runner calls = %+v", runner.envCalls)
	}
	call := runner.envCalls[0]
	if call.name != "git" || len(call.args) != 6 ||
		!strings.HasPrefix(call.args[0], "--git-dir=") ||
		call.args[1] != "push" ||
		call.args[2] != "--porcelain" ||
		call.args[3] != "--force-with-lease=refs/heads/goobers/implementation/run-1:validated-sha" ||
		call.args[4] != "https://github.example/acme/app.git" ||
		call.args[5] != ":refs/heads/goobers/implementation/run-1" {
		t.Fatalf("push call = %+v", call)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:secret-token"))
	if !containsString(call.env, "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth) {
		t.Fatal("push environment does not contain the injected authorization header")
	}
}

func TestGitHubProviderDeleteBranchPreservesStaleLease(t *testing.T) {
	runner := &fakeEnvironmentRunner{
		envOutput: []byte("! refs/heads/run:refs/heads/run [rejected] (stale info)\n"),
		envErr:    errors.New("exit status 1"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/app/git/ref/heads/goobers/implementation/run-1":
			writeJSON(t, w, map[string]any{
				"ref": "refs/heads/goobers/implementation/run-1",
				"object": map[string]string{
					"sha": "concurrent-sha",
				},
			})
		case "/repos/acme/app/activity":
			writeJSON(t, w, []map[string]any{})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.Runner = runner
		p.BaseURL = server.URL
	})
	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app"},
		Name:        "goobers/implementation/run-1",
		ExpectedSHA: "validated-sha",
	})
	var tipChanged *BranchTipChangedError
	if !errors.As(err, &tipChanged) {
		t.Fatalf("DeleteBranch error = %v, want BranchTipChangedError", err)
	}
	if result.Deleted {
		t.Fatal("Deleted = true for a stale lease")
	}
}

func TestGitHubProviderDeleteBranchLeaseTreatsConcurrentDeletionAsAbsent(t *testing.T) {
	runner := &fakeEnvironmentRunner{
		envOutput: []byte("! refs/heads/run:refs/heads/run [rejected] (stale info)\n"),
		envErr:    errors.New("exit status 1"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/app/git/ref/heads/goobers/implementation/run-1" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	provider := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.Runner = runner
		p.BaseURL = server.URL
	})

	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app"},
		Name:        "goobers/implementation/run-1",
		ExpectedSHA: "validated-sha",
	})
	if err != nil {
		t.Fatalf("DeleteBranch returned error: %v", err)
	}
	if result.Deleted {
		t.Fatal("Deleted = true for an already absent branch")
	}
}

func TestGitHubProviderDeleteBranchLeaseClassifiesRateLimits(t *testing.T) {
	tests := []struct {
		name          string
		output        string
		wantStatus    int
		wantSecondary bool
	}{
		{
			name:       "too many requests",
			output:     "fatal: unable to access repository: The requested URL returned error: 429\n",
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:          "secondary limit",
			output:        "remote: You have exceeded a secondary rate limit. Please wait before retrying.\nfatal: HTTP 403\n",
			wantStatus:    http.StatusForbidden,
			wantSecondary: true,
		},
		{
			name:          "abuse limit",
			output:        "remote: You have triggered an abuse detection mechanism.\nfatal: HTTP 403\n",
			wantStatus:    http.StatusForbidden,
			wantSecondary: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeEnvironmentRunner{
				envOutput: []byte(tc.output),
				envErr:    errors.New("exit status 1"),
			}
			provider := NewGitHubProvider("token", func(p *GitHubProvider) {
				p.Runner = runner
			})

			result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
				Repository:  RepositoryRef{Owner: "acme", Name: "app"},
				Name:        "goobers/implementation/run-1",
				ExpectedSHA: "validated-sha",
			})
			var rateLimitErr *RateLimitError
			if !errors.As(err, &rateLimitErr) {
				t.Fatalf("DeleteBranch error = %v, want RateLimitError", err)
			}
			if rateLimitErr.Status != tc.wantStatus || rateLimitErr.Secondary != tc.wantSecondary {
				t.Fatalf("RateLimitError = %+v", rateLimitErr)
			}
			if result.Deleted {
				t.Fatal("Deleted = true for a rate-limited push")
			}
		})
	}
}

func TestGitHubProviderDeleteBranchLeaseRejectsConcurrentPush(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	workDir := filepath.Join(t.TempDir(), "work")
	runGitTest(t, "init", "--bare", "--quiet", remoteDir)
	runGitTest(t, "init", "--quiet", workDir)
	runGitTest(t, "-C", workDir, "config", "user.name", "Goobers Test")
	runGitTest(t, "-C", workDir, "config", "user.email", "goobers@example.test")

	tracked := filepath.Join(workDir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("validated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, "-C", workDir, "add", "tracked.txt")
	runGitTest(t, "-C", workDir, "commit", "--quiet", "-m", "validated tip")
	ref := "refs/heads/goobers/implementation/run-1"
	runGitTest(t, "-C", workDir, "push", "--quiet", remoteDir, "HEAD:"+ref)
	validatedSHA := strings.TrimSpace(runGitTest(t, "--git-dir="+remoteDir, "rev-parse", ref))

	if err := os.WriteFile(tracked, []byte("concurrent push\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, "-C", workDir, "commit", "--quiet", "-am", "concurrent push")
	runGitTest(t, "-C", workDir, "push", "--quiet", remoteDir, "HEAD:"+ref)
	concurrentSHA := strings.TrimSpace(runGitTest(t, "--git-dir="+remoteDir, "rev-parse", ref))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/app/git/ref/heads/goobers/implementation/run-1":
			writeJSON(t, w, map[string]any{
				"ref":    ref,
				"object": map[string]string{"sha": concurrentSHA},
			})
		case "/repos/acme/app/activity":
			writeJSON(t, w, []map[string]any{})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	provider := NewGitHubProvider("", func(p *GitHubProvider) {
		p.BaseURL = server.URL
	})
	result, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{
		Repository:  RepositoryRef{Owner: "acme", Name: "app", URL: remoteDir},
		Name:        "goobers/implementation/run-1",
		ExpectedSHA: validatedSHA,
	})
	var tipChanged *BranchTipChangedError
	if !errors.As(err, &tipChanged) {
		t.Fatalf("DeleteBranch error = %v, want BranchTipChangedError", err)
	}
	if result.Deleted {
		t.Fatal("Deleted = true for a stale lease")
	}
	if got := strings.TrimSpace(runGitTest(t, "--git-dir="+remoteDir, "rev-parse", ref)); got != concurrentSHA {
		t.Fatalf("remote tip = %s, want concurrent tip %s", got, concurrentSHA)
	}
}

func TestGitHubProviderListBranchesIsBoundedAndCursorable(t *testing.T) {
	var calls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/acme/app/git/matching-refs/heads/goobers" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Fatalf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
		}
		if calls == 1 {
			w.Header().Set("Link", "<"+server.URL+r.URL.Path+"?page=2&per_page=100>; rel=\"next\"")
			writeJSON(t, w, []map[string]interface{}{
				{"ref": "refs/tags/goobers/workflow/run-tag", "object": map[string]string{"sha": "sha-tag"}},
				{"ref": "refs/heads/goobers-other", "object": map[string]string{"sha": "sha-other"}},
				{"ref": "refs/heads/goobers/workflow/run-a", "url": "ref-a", "object": map[string]string{"sha": "sha-a"}},
			})
			return
		}
		if r.URL.Query().Get("page") != "2" {
			t.Fatalf("page = %q, want 2", r.URL.Query().Get("page"))
		}
		writeJSON(t, w, []map[string]interface{}{
			{"ref": "refs/heads/goobers/workflow/run-c", "url": "ref-c", "object": map[string]string{"sha": "sha-c"}},
			{"ref": "refs/tags/goobers/workflow/run-tag", "object": map[string]string{"sha": "sha-tag"}},
			{"ref": "refs/heads/goobers/workflow/run-b", "url": "ref-b", "object": map[string]string{"sha": "sha-b"}},
		})
	}))
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	branches, err := provider.ListBranches(context.Background(), ListBranchesRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Prefix:     "goobers/",
		After:      "goobers/workflow/run-a",
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) != 1 || branches[0].Name != "goobers/workflow/run-b" || branches[0].SHA != "sha-b" || branches[0].URL != "ref-b" {
		t.Fatalf("branches = %+v", branches)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestGitHubProviderListBranchesValidatesBound(t *testing.T) {
	provider := NewGitHubProvider("token")
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	if _, err := provider.ListBranches(context.Background(), ListBranchesRequest{Repository: repo, Limit: 1}); err == nil {
		t.Fatal("missing prefix: err = nil")
	}
	if _, err := provider.ListBranches(context.Background(), ListBranchesRequest{Repository: repo, Prefix: "goobers/"}); err == nil {
		t.Fatal("zero limit: err = nil")
	}
}

func TestGitHubProviderGetBranch(t *testing.T) {
	activityAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		status    int
		wantFound bool
		wantErr   bool
	}{
		{name: "found", status: http.StatusOK, wantFound: true},
		{name: "absent", status: http.StatusNotFound},
		{name: "provider failure", status: http.StatusForbidden, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodGet)
				switch r.URL.Path {
				case "/repos/acme/app/git/ref/heads/goobers/implementation/run-1":
					w.WriteHeader(tc.status)
					if tc.status != http.StatusOK {
						return
					}
					writeJSON(t, w, map[string]interface{}{
						"ref":    "refs/heads/goobers/implementation/run-1",
						"url":    "ref-url",
						"object": map[string]string{"sha": "tip-sha"},
					})
				case "/repos/acme/app/activity":
					if tc.status != http.StatusOK {
						t.Fatal("activity requested after missing or failed ref lookup")
					}
					if got := r.URL.Query().Get("ref"); got != "refs/heads/goobers/implementation/run-1" {
						t.Fatalf("activity ref = %q", got)
					}
					if got := r.URL.Query().Get("direction"); got != "desc" {
						t.Fatalf("activity direction = %q", got)
					}
					if got := r.URL.Query().Get("per_page"); got != "1" {
						t.Fatalf("activity per_page = %q", got)
					}
					writeJSON(t, w, []map[string]interface{}{{
						"ref":       "refs/heads/goobers/implementation/run-1",
						"timestamp": activityAt,
					}})
				default:
					t.Fatalf("path = %q", r.URL.Path)
				}
			}))
			defer server.Close()

			provider := NewGitHubProvider("token", func(p *GitHubProvider) {
				p.BaseURL = server.URL
				p.maxRetries = 0
			})
			branch, found, err := provider.GetBranch(
				context.Background(),
				RepositoryRef{Owner: "acme", Name: "app"},
				"goobers/implementation/run-1",
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("GetBranch error = %v, wantErr %t", err, tc.wantErr)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %t, want %t", found, tc.wantFound)
			}
			if found && (branch.Name != "goobers/implementation/run-1" || branch.SHA != "tip-sha" ||
				branch.URL != "ref-url" || branch.LastActivityAt == nil || !branch.LastActivityAt.Equal(activityAt)) {
				t.Fatalf("branch = %+v", branch)
			}
		})
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
	if _, err := provider.DeleteBranch(context.Background(), DeleteBranchRequest{Repository: repo}); err == nil {
		t.Fatal("expected missing branch name to return an error")
	}
	if _, err := provider.Subscribe(context.Background(), TriggerSubscription{Kind: TriggerWebhook, Repository: repo}); err == nil {
		t.Fatal("expected unsupported webhook subscription to return an error")
	}
}

func TestGitHubProviderDeletesBranchAndRecordsMutation(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/app/git/refs/heads/goobers/implementation/run-1", func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodDelete)
				w.WriteHeader(status)
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			recorder := &recordingRecorder{}
			provider := NewGitHubProvider("token",
				func(p *GitHubProvider) { p.BaseURL = server.URL },
				WithMutationRecorder(recorder),
			)
			req := DeleteBranchRequest{
				Repository: RepositoryRef{Owner: "acme", Name: "app"},
				Name:       "goobers/implementation/run-1",
			}
			if _, err := provider.DeleteBranch(context.Background(), req); err != nil {
				t.Fatalf("DeleteBranch returned error: %v", err)
			}
			ref, ok := recorder.last()
			if !ok || ref.Ref != "acme/app@goobers/implementation/run-1" || ref.Operation != "delete" {
				t.Fatalf("recorded ref = %+v (ok=%v), want branch deletion", ref, ok)
			}
		})
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
			"mergeable_state": "unstable",
			"html_url":        "https://github.com/acme/app/pull/9",
			"head":            map[string]interface{}{"sha": "deadbeef"},
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
				{"name": "e2e", "status": "in_progress", "html_url": "https://ci/e2e", "output": map[string]interface{}{"summary": "waiting for a runner"}},
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
	wantChecks := []CheckDetail{
		{Name: "legacy-ci", State: CheckStateFailing, URL: "https://ci/legacy", Summary: "boom"},
		{Name: "unit-tests", State: CheckStatePassing, URL: "https://ci/unit"},
		{Name: "e2e", State: CheckStatePending, URL: "https://ci/e2e", Summary: "waiting for a runner"},
	}
	for i := range wantChecks {
		if result.Checks[i] != wantChecks[i] {
			t.Fatalf("Checks[%d] = %+v, want %+v", i, result.Checks[i], wantChecks[i])
		}
	}
	if result.Mergeable == nil || !*result.Mergeable {
		t.Fatalf("Mergeable = %v, want true", result.Mergeable)
	}
	if result.MergeableState != MergeableStateUnstable {
		t.Fatalf("MergeableState = %q, want %q (mergeable_state passed through for #961's advisory-check gating)", result.MergeableState, MergeableStateUnstable)
	}
	if len(result.CommentsSince) != 1 || result.CommentsSince[0].Author != "carol" {
		t.Fatalf("CommentsSince = %#v", result.CommentsSince)
	}
}

func TestNormalizeCheckRunStateStartupFailure(t *testing.T) {
	if got := normalizeCheckRunState("completed", "startup_failure"); got != CheckStateFailing {
		t.Fatalf("normalizeCheckRunState() = %q, want %q", got, CheckStateFailing)
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

func TestGitHubProviderGetPullRequestReadsExactPRWithoutChecks(t *testing.T) {
	mergedAt := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/10", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"number": 10, "state": "closed", "merged_at": mergedAt.Format(time.RFC3339),
			"html_url": "https://github.com/acme/app/pull/10", "body": "Fixes #7",
			"head": map[string]interface{}{"ref": "goobers/implementation/run-1", "sha": "aaa111"},
			"base": map[string]interface{}{"ref": "main", "sha": "base111"},
		})
	})
	mux.HandleFunc("/repos/acme/app/commits/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("exact PR read must not resolve check state, got %s", r.URL.Path)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("pr-token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	pr, err := provider.GetPullRequest(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "10")
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if pr.Number != 10 || pr.State != "closed" || !pr.Merged || pr.Body != "Fixes #7" {
		t.Fatalf("GetPullRequest = %+v, want exact merged PR state and body", pr)
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

// TestGitHubProviderListPullRequestsSkipCheckState is issue #523's list-cost
// contract: with SkipCheckState set, the list makes exactly one kind of
// request (the pulls list itself — no per-candidate status/check-runs
// round-trips) and leaves CheckState empty for the caller to resolve on
// demand via RefCheckState.
func TestGitHubProviderListPullRequestsSkipCheckState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{
				"number": 10, "html_url": "https://github.com/acme/app/pull/10", "draft": false,
				"updated_at": "2026-07-15T00:00:00Z",
				"head":       map[string]interface{}{"ref": "goobers/implementation/run-1", "sha": "aaa111"},
				"base":       map[string]interface{}{"ref": "main", "sha": "base111"},
			},
		})
	})
	mux.HandleFunc("/repos/acme/app/commits/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("SkipCheckState list must not resolve check state, got %s", r.URL.Path)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	out, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Base: "main", HeadPrefix: "goobers/",
		SkipCheckState: true,
	})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}

	if len(out) != 1 || out[0].CheckState != "" {
		t.Fatalf("out = %+v, want one summary with empty CheckState", out)
	}
}

func TestGitHubProviderListRecentlyClosedPullRequests(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-31 * 24 * time.Hour)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "closed" {
			t.Fatalf("state query = %q, want closed", got)
		}
		if got := r.URL.Query().Get("sort"); got != "updated" {
			t.Fatalf("sort query = %q, want updated", got)
		}
		if got := r.URL.Query().Get("direction"); got != "desc" {
			t.Fatalf("direction query = %q, want desc", got)
		}
		writeJSON(t, w, []map[string]interface{}{
			{
				"number": 20, "state": "closed", "merged_at": now.Format(time.RFC3339),
				"closed_at": now.Format(time.RFC3339), "updated_at": now.Format(time.RFC3339),
				"html_url":         "https://github.com/acme/app/pull/20",
				"merge_commit_sha": "merge20",
				"head":             map[string]interface{}{"ref": "goobers/implementation/run-20", "sha": "head20"},
				"base":             map[string]interface{}{"ref": "main", "sha": "base20"},
			},
			{
				"number": 19, "state": "closed", "closed_at": old.Format(time.RFC3339),
				"updated_at": old.Format(time.RFC3339),
				"head":       map[string]interface{}{"ref": "goobers/implementation/run-19", "sha": "head19"},
				"base":       map[string]interface{}{"ref": "main", "sha": "base19"},
			},
		})
	})
	mux.HandleFunc("/repos/acme/app/commits/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("recently closed listing must not resolve check state, got %s", r.URL.Path)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	out, err := provider.ListRecentlyClosedPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Base: "main", HeadPrefix: "goobers/",
	}, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("ListRecentlyClosedPullRequests: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out = %+v, want only the recently merged PR", out)
	}
	if out[0].Number != 20 || out[0].State != "closed" || !out[0].Merged || out[0].MergeSHA != "merge20" {
		t.Fatalf("out[0] = %+v, want PR #20 with merged current state", out[0])
	}
}

// TestGitHubProviderRefCheckState is RefCheckState's on-demand half of
// #523's contract: the same combined status + check-runs normalization
// ListPullRequests applies by default, resolvable per ref.
func TestGitHubProviderRefCheckState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/commits/aaa111/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"state": "failure", "statuses": []map[string]interface{}{
			{"context": "ci", "state": "failure"},
		}})
	})
	mux.HandleFunc("/repos/acme/app/commits/aaa111/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"check_runs": []map[string]interface{}{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	state, err := provider.RefCheckState(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "aaa111")
	if err != nil {
		t.Fatalf("RefCheckState: %v", err)
	}
	if state != CheckStateFailing {
		t.Fatalf("state = %q, want failing", state)
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
			{
				"filename": "cmd/goobers/new.go", "previous_filename": "cmd/goobers/old.go",
				"status": "renamed", "additions": 40, "deletions": 0,
			},
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
	if files[1].PreviousPath != "cmd/goobers/old.go" {
		t.Fatalf("file[1] = %+v, want previous rename path", files[1])
	}
}

// TestGitHubProviderPullRequestMergeable is issue #715's post-merge triage
// signal: a single-PR detail GET resolving just the mergeable field,
// distinct from PollPullRequest's heavier review-decision/check-state/
// comments bundle.
func TestGitHubProviderPullRequestMergeable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/12", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"number": 12, "mergeable": false})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	mergeable, err := provider.PullRequestMergeable(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "12")
	if err != nil {
		t.Fatalf("PullRequestMergeable: %v", err)
	}
	if mergeable == nil || *mergeable {
		t.Fatalf("mergeable = %v, want a pointer to false", mergeable)
	}
}

// TestGitHubProviderPullRequestMergeableNullIsUnknown proves GitHub's
// still-computing null response round-trips as a nil pointer, not a false
// or true — issue #715's caller (postmerge.go's triageSibling) depends on
// telling "unknown" apart from "known conflicted" to avoid false-positive
// labeling a PR whose mergeability GitHub simply hasn't computed yet.
func TestGitHubProviderPullRequestMergeableNullIsUnknown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/13", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"number": 13, "mergeable": nil})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	mergeable, err := provider.PullRequestMergeable(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "13")
	if err != nil {
		t.Fatalf("PullRequestMergeable: %v", err)
	}
	if mergeable != nil {
		t.Fatalf("mergeable = %v, want nil (still computing)", *mergeable)
	}
}

func TestGitHubProviderRepositoryFileContentReadsRef(t *testing.T) {
	want := strings.Repeat("first\nsecond\n", 100_000)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/contents/portal/src/App.tsx", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		if got := r.URL.Query().Get("ref"); got != "head-sha" {
			t.Fatalf("ref = %q, want head-sha", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github.raw+json" {
			t.Fatalf("Accept = %q, want raw GitHub content media type", got)
		}
		_, _ = w.Write([]byte(want))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	content, err := provider.RepositoryFileContent(
		context.Background(),
		RepositoryRef{Owner: "acme", Name: "app"},
		"portal/src/App.tsx",
		"head-sha",
	)
	if err != nil {
		t.Fatalf("RepositoryFileContent: %v", err)
	}
	if got := string(content); got != want {
		t.Fatalf("content length = %d, want %d", len(got), len(want))
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
		ExpectedHeadSHA: "deadbeef", CommitTitle: "Improve merge history",
		CommitMessage: "merged by merge-review", MergeMethod: MergeMethodRebase,
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
	if gotBody["commit_title"] != "Improve merge history" {
		t.Fatalf("request body commit_title = %v", gotBody["commit_title"])
	}
	if gotBody["commit_message"] != "merged by merge-review" {
		t.Fatalf("request body commit_message = %v", gotBody["commit_message"])
	}
	if gotBody["merge_method"] != "rebase" {
		t.Fatalf("request body merge_method = %v, want rebase", gotBody["merge_method"])
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

// TestGitHubProviderDetectMergePolicyDirectWhenNoMergeQueueRule proves the
// default read (#758): a branch whose rules-for-branch response has no
// "merge_queue"-typed rule (an empty array, or any other rule types) is
// direct-merge — the vast majority of repos today, since none has adopted
// GitHub's merge queue yet.
func TestGitHubProviderDetectMergePolicyDirectWhenNoMergeQueueRule(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, []map[string]interface{}{
			{"type": "required_status_checks"},
			{"type": "pull_request"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Branch: "main",
	})
	if err != nil {
		t.Fatalf("DetectMergePolicy returned error: %v", err)
	}
	if result.Policy != MergePolicyDirect {
		t.Fatalf("Policy = %q, want %q", result.Policy, MergePolicyDirect)
	}
}

// TestGitHubProviderDetectMergePolicyMergeQueueWhenRulePresent proves the
// merge_queue-detection half of #758: a "merge_queue"-typed rule anywhere
// in the rules-for-branch response means this branch requires the queue,
// regardless of what other rule types are also present.
func TestGitHubProviderDetectMergePolicyMergeQueueWhenRulePresent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]interface{}{
			{"type": "required_status_checks"},
			{"type": "merge_queue"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Branch: "main",
	})
	if err != nil {
		t.Fatalf("DetectMergePolicy returned error: %v", err)
	}
	if result.Policy != MergePolicyMergeQueue {
		t.Fatalf("Policy = %q, want %q", result.Policy, MergePolicyMergeQueue)
	}
}

func TestGitHubProviderDetectMergePolicyRequiresBranch(t *testing.T) {
	provider := NewGitHubProvider("token")
	if _, err := provider.DetectMergePolicy(context.Background(), RepoMergePolicyRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
	}); err == nil {
		t.Fatal("expected an error for a missing branch")
	}
}

// graphQLStub serves the /graphql endpoint the enqueue path uses, routing
// by whether the request carries the mutation or the lookup query, and
// recording every request body so a test can assert what went on the wire.
// It deliberately registers NO REST merge handler: any fall-through to
// PUT .../pulls/{n}/merge 404s loudly, which is the #882 regression guard.
type graphQLStub struct {
	t        *testing.T
	lookup   map[string]interface{}
	mutation map[string]interface{}
	bodies   []map[string]interface{}
}

func (s *graphQLStub) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(s.t, r, http.MethodPost)
		var body map[string]interface{}
		decodeJSON(s.t, r, &body)
		s.bodies = append(s.bodies, body)
		query, _ := body["query"].(string)
		if strings.Contains(query, "enqueuePullRequest(input:") {
			writeJSON(s.t, w, map[string]interface{}{"data": s.mutation})
			return
		}
		writeJSON(s.t, w, map[string]interface{}{"data": s.lookup})
	})
	return httptest.NewServer(mux)
}

// variables returns the variables sent with the nth recorded request.
func (s *graphQLStub) variables(n int) map[string]interface{} {
	s.t.Helper()
	if n >= len(s.bodies) {
		s.t.Fatalf("only %d graphql requests were made, wanted request %d", len(s.bodies), n)
	}
	vars, _ := s.bodies[n]["variables"].(map[string]interface{})
	return vars
}

// lookupResponse builds the enqueue lookup query's data payload.
func lookupResponse(pr map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"repository": map[string]interface{}{"pullRequest": pr}}
}

// TestGitHubProviderEnqueuePullRequestUsesGraphQLMutation is #882's
// regression test. The enqueue previously went through the REST merge
// endpoint on the assumption that GitHub silently converts it into an
// enqueue for a queue-required branch; live evidence was a flat 405
// ("Changes must be made through the merge queue"), because no REST
// endpoint enqueues anything — the GraphQL mutation is the only one.
//
// The stub serves ONLY /graphql, so a regression back to the REST path
// fails this test with a 404 rather than passing quietly.
func TestGitHubProviderEnqueuePullRequestUsesGraphQLMutation(t *testing.T) {
	stub := &graphQLStub{
		t: t,
		lookup: lookupResponse(map[string]interface{}{
			"id": "PR_node", "merged": false, "mergeCommit": nil, "mergeQueueEntry": nil,
		}),
		mutation: map[string]interface{}{
			"enqueuePullRequest": map[string]interface{}{
				"mergeQueueEntry": map[string]interface{}{"state": "QUEUED", "position": 2},
			},
		},
	}
	server := stub.server()
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(rec))
	result, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9", ExpectedHeadSHA: "deadbeef",
		MergeMethod: MergeMethodSquash,
	})
	if err != nil {
		t.Fatalf("EnqueuePullRequest returned error: %v", err)
	}
	// Enqueueing never merges inline, so Merged is false by construction —
	// the queue entry is what the caller polls next.
	if result.Merged || result.Number != 9 {
		t.Fatalf("result = %#v, want Merged=false Number=9", result)
	}
	if len(stub.bodies) != 2 {
		t.Fatalf("made %d graphql requests, want 2 (node-id lookup, then mutation)", len(stub.bodies))
	}
	if got := stub.variables(0)["number"]; got != float64(9) {
		t.Fatalf("lookup number = %v, want 9", got)
	}
	mutationVars := stub.variables(1)
	if got := mutationVars["pullRequestId"]; got != "PR_node" {
		t.Fatalf("mutation pullRequestId = %v, want the node id from the lookup", got)
	}
	// The SHA pin survives the move off REST: it is spelled expectedHeadOid
	// on the mutation, and dropping it would let the queue land a commit the
	// merge conjuncts were never checked against.
	if got := mutationVars["expectedHeadOid"]; got != "deadbeef" {
		t.Fatalf("mutation expectedHeadOid = %v, want deadbeef", got)
	}
	ref, ok := rec.last()
	if !ok {
		t.Fatalf("expected a recorded external ref")
	}
	if ref.Operation != "enqueue" {
		t.Fatalf("Operation = %q, want enqueue (not merge — this pull request is not yet merged)", ref.Operation)
	}
}

// TestGitHubProviderEnqueuePullRequestAlreadyMergedIsNotAnError covers a
// retried stage attempt whose pull request the queue landed in the
// meantime: the desired end state already holds, so this reports the real
// merge commit rather than erroring on a mutation GitHub would reject.
func TestGitHubProviderEnqueuePullRequestAlreadyMergedIsNotAnError(t *testing.T) {
	stub := &graphQLStub{
		t: t,
		lookup: lookupResponse(map[string]interface{}{
			"id": "PR_node", "merged": true,
			"mergeCommit":     map[string]interface{}{"oid": "abc123"},
			"mergeQueueEntry": nil,
		}),
	}
	server := stub.server()
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
	})
	if err != nil {
		t.Fatalf("EnqueuePullRequest returned error: %v", err)
	}
	// The merge commit, not the head SHA — under the squash method a merge
	// queue requires, they are never the same commit.
	if !result.Merged || result.MergeSHA != "abc123" {
		t.Fatalf("result = %#v, want Merged=true MergeSHA=abc123", result)
	}
	if len(stub.bodies) != 1 {
		t.Fatalf("made %d graphql requests, want 1 (no mutation for an already-merged pull request)", len(stub.bodies))
	}
}

// TestGitHubProviderEnqueuePullRequestAlreadyEnqueuedIsIdempotent covers the
// other retry case: the pull request is already in the queue, so the
// enqueue is a successful no-op instead of a duplicate-entry error that
// would fail an otherwise healthy landing.
func TestGitHubProviderEnqueuePullRequestAlreadyEnqueuedIsIdempotent(t *testing.T) {
	stub := &graphQLStub{
		t: t,
		lookup: lookupResponse(map[string]interface{}{
			"id": "PR_node", "merged": false, "mergeCommit": nil,
			"mergeQueueEntry": map[string]interface{}{"state": "AWAITING_CHECKS", "position": 0},
		}),
	}
	server := stub.server()
	defer server.Close()

	rec := &recordingRecorder{}
	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL }, WithMutationRecorder(rec))
	result, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
	})
	if err != nil {
		t.Fatalf("EnqueuePullRequest returned error: %v", err)
	}
	if result.Merged || result.Number != 9 {
		t.Fatalf("result = %#v, want Merged=false Number=9", result)
	}
	if len(stub.bodies) != 1 {
		t.Fatalf("made %d graphql requests, want 1 (no mutation for an already-enqueued pull request)", len(stub.bodies))
	}
	ref, ok := rec.last()
	if !ok {
		t.Fatalf("expected a recorded external ref for the already-enqueued pull request")
	}
	if ref.Operation != "enqueue" {
		t.Fatalf("Operation = %q, want enqueue", ref.Operation)
	}
}

// TestGitHubProviderGraphQLSurfacesErrorsArray pins the one way GraphQL
// differs from every REST call in this provider: a failure arrives as HTTP
// 200 with a non-empty errors array, which p.do's status check alone would
// read as success and then silently decode into a zero value.
func TestGitHubProviderGraphQLSurfacesErrorsArray(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"data": nil,
			"errors": []map[string]interface{}{
				{"type": "FORBIDDEN", "message": "Merge queue is not enabled for this branch"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	_, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
	})
	if err == nil {
		t.Fatal("expected an error for a GraphQL errors-array response served as HTTP 200")
	}
	if !strings.Contains(err.Error(), "Merge queue is not enabled") {
		t.Fatalf("error = %v, want it to carry GitHub's own message", err)
	}
}

// TestGitHubProviderEnqueuePullRequestRejectsNonNumericPullID proves the
// pull id is resolved to a node id via its number, so a non-numeric id
// fails with that reason rather than as an opaque GitHub rejection.
func TestGitHubProviderEnqueuePullRequestRejectsNonNumericPullID(t *testing.T) {
	provider := NewGitHubProvider("token")
	if _, err := provider.EnqueuePullRequest(context.Background(), EnqueuePullRequestRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "not-a-number",
	}); err == nil {
		t.Fatal("expected an error for a non-numeric pull id")
	}
}

// TestGitHubProviderPollMergeQueueEntryStates covers every classification a
// poll can report. The "absent" case is issue #885's headline: GitHub
// leaves an evicted pull request OPEN and just removes its queue entry, so
// the old REST classification (which only looked at pr.State == "closed")
// reported Pending forever and #758's needs-remediation routing could never
// fire.
func TestGitHubProviderPollMergeQueueEntryStates(t *testing.T) {
	cases := []struct {
		name  string
		pr    map[string]interface{}
		want  MergeQueueEntryState
		wantS string
	}{
		{
			name: "merged reports the merge commit, not the head SHA",
			pr: map[string]interface{}{
				"state": "MERGED", "merged": true,
				"mergeCommit": map[string]interface{}{"oid": "squashsha"}, "mergeQueueEntry": nil,
			},
			want: MergeQueueEntryMerged, wantS: "squashsha",
		},
		{
			name: "closed without merging is evicted",
			pr:   map[string]interface{}{"state": "CLOSED", "merged": false, "mergeCommit": nil, "mergeQueueEntry": nil},
			want: MergeQueueEntryEvicted,
		},
		{
			name: "open with a live entry is pending",
			pr: map[string]interface{}{
				"state": "OPEN", "merged": false, "mergeCommit": nil,
				"mergeQueueEntry": map[string]interface{}{"state": "AWAITING_CHECKS", "position": 3},
			},
			want: MergeQueueEntryPending,
		},
		{
			name: "open with no entry is absent — what a real eviction looks like",
			pr:   map[string]interface{}{"state": "OPEN", "merged": false, "mergeCommit": nil, "mergeQueueEntry": nil},
			want: MergeQueueEntryAbsent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &graphQLStub{t: t, lookup: lookupResponse(tc.pr)}
			server := stub.server()
			defer server.Close()

			provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
			result, err := provider.PollMergeQueueEntry(context.Background(), PollMergeQueueEntryRequest{
				Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
			})
			if err != nil {
				t.Fatalf("PollMergeQueueEntry returned error: %v", err)
			}
			if result.State != tc.want {
				t.Fatalf("State = %q, want %q", result.State, tc.want)
			}
			if result.MergeSHA != tc.wantS {
				t.Fatalf("MergeSHA = %q, want %q", result.MergeSHA, tc.wantS)
			}
		})
	}
}

// TestGitHubProviderPollMergeQueueEntrySurfacesQueueProgress proves a
// pending entry carries the queue's own view of it, so "still pending" is
// legible in logs (position 3, awaiting checks) rather than opaque.
func TestGitHubProviderPollMergeQueueEntrySurfacesQueueProgress(t *testing.T) {
	stub := &graphQLStub{t: t, lookup: lookupResponse(map[string]interface{}{
		"state": "OPEN", "merged": false, "mergeCommit": nil,
		"mergeQueueEntry": map[string]interface{}{"state": "AWAITING_CHECKS", "position": 3},
	})}
	server := stub.server()
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.PollMergeQueueEntry(context.Background(), PollMergeQueueEntryRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "9",
	})
	if err != nil {
		t.Fatalf("PollMergeQueueEntry returned error: %v", err)
	}
	if result.QueueState != "AWAITING_CHECKS" || result.QueuePosition != 3 {
		t.Fatalf("queue progress = %q/%d, want AWAITING_CHECKS/3", result.QueueState, result.QueuePosition)
	}
}

// TestGitHubProviderPollPullRequestSurfacesMergeInputs is #360's regression:
// a conjunctive auto-merge action re-checks not-draft and the SHA-pin (D6)
// against PollPullRequest's live result. Branch cleanup also needs the head
// repository because a fork PR's branch does not live in the base repository.
func TestGitHubProviderPollPullRequestSurfacesMergeInputs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"number": 9, "state": "open", "draft": true, "html_url": "https://github.com/acme/app/pull/9",
			"title": "Improve merge history",
			"body":  "Implements the thing.\n\nFixes #42",
			"head": map[string]interface{}{
				"ref": "feature", "sha": "headsha123",
				"repo": map[string]interface{}{
					"name": "app-fork", "html_url": "https://github.com/contributor/app-fork",
					"owner": map[string]string{"login": "contributor"},
				},
			},
			"base": map[string]interface{}{"sha": "basesha456", "ref": "main"},
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
	if result.Title != "Improve merge history" {
		t.Fatalf("Title = %q, want Improve merge history", result.Title)
	}
	if result.HeadSHA != "headsha123" {
		t.Fatalf("HeadSHA = %q, want headsha123", result.HeadSHA)
	}
	if result.HeadRepository == nil || result.HeadRepository.Owner != "contributor" || result.HeadRepository.Name != "app-fork" {
		t.Fatalf("HeadRepository = %+v, want contributor/app-fork", result.HeadRepository)
	}
	if result.BaseSHA != "basesha456" {
		t.Fatalf("BaseSHA = %q, want basesha456", result.BaseSHA)
	}
	if result.BaseBranch != "main" {
		t.Fatalf("BaseBranch = %q, want main", result.BaseBranch)
	}
	if result.Body != "Implements the thing.\n\nFixes #42" {
		t.Fatalf("Body = %q", result.Body)
	}
}

// TestGitHubProviderListPullRequestsFiltersByBase is #361's regression: the
// post-merge fan-out needs every OTHER open PR targeting the same base
// branch as a just-merged PR.
func TestGitHubProviderListPullRequestsFiltersByBase(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Fatalf("state query = %q, want open", got)
		}
		if got := r.URL.Query().Get("base"); got != "main" {
			t.Fatalf("base query = %q, want main", got)
		}
		writeJSON(t, w, []map[string]interface{}{
			{"number": 10, "html_url": "https://github.com/acme/app/pull/10", "head": map[string]interface{}{"ref": "goobers/impl/run-a"}, "base": map[string]interface{}{"ref": "main"}},
			{"number": 11, "html_url": "https://github.com/acme/app/pull/11", "head": map[string]interface{}{"ref": "goobers/impl/run-b"}, "base": map[string]interface{}{"ref": "main"}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	result, err := provider.ListPullRequests(context.Background(), ListPullRequestsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, Base: "main",
	})
	if err != nil {
		t.Fatalf("ListPullRequests returned error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2: %#v", len(result), result)
	}
	if result[0].Number != 10 || result[0].Head != "goobers/impl/run-a" || result[0].Base != "main" {
		t.Fatalf("result[0] = %#v", result[0])
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

type runnerCall struct {
	name string
	args []string
	env  []string
}

type fakeEnvironmentRunner struct {
	calls     []runnerCall
	envCalls  []runnerCall
	envOutput []byte
	envErr    error
}

func (f *fakeEnvironmentRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, runnerCall{name: name, args: append([]string(nil), args...)})
	return nil, nil
}

func (f *fakeEnvironmentRunner) RunWithEnv(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.envCalls = append(f.envCalls, runnerCall{
		name: name,
		args: append([]string(nil), args...),
		env:  append([]string(nil), env...),
	})
	return f.envOutput, f.envErr
}

func slicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func runGitTest(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.autocrlf",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.safecrlf",
		"GIT_CONFIG_VALUE_1=false",
	)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out)
}
