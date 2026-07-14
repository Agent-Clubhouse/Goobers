// Regression coverage for issue #139: the GitHub provider followed no
// pagination, so a claim breadcrumb, failing check, or changes-requested review
// beyond the first (default 30-item) page was silently invisible. These tests
// serve genuinely multi-page responses (real Link: rel="next" headers) from an
// httptest server and assert the provider now reads the whole result set. Plus
// a transient-5xx retry test. All run against the real HTTP client, no network.
package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// paginatedJSON serves pages[page-1] for ?page=N, setting a Link rel="next"
// header (pointing back at this same test server) until the last page — exactly
// what GitHub's REST API does. Each element of pages is a marshaled JSON body.
func paginatedJSON(t *testing.T, pages [][]byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				page = n
			}
		}
		if page < 1 || page > len(pages) {
			http.Error(w, "bad page", http.StatusBadRequest)
			return
		}
		if page < len(pages) {
			next := *r.URL
			q := next.Query()
			q.Set("page", strconv.Itoa(page+1))
			next.RawQuery = q.Encode()
			nextURL := (&url.URL{Scheme: "http", Host: r.Host, Path: next.Path, RawQuery: next.RawQuery}).String()
			w.Header().Set("Link", "<"+nextURL+`>; rel="next"`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pages[page-1])
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

func paginationProvider(t *testing.T, h http.Handler) (*GitHubProvider, RepositoryRef) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.BaseURL = srv.URL
		p.sleep = func(context.Context, time.Duration) error { return nil } // no real backoff waits
	})
	return p, RepositoryRef{Owner: "acme", Name: "app"}
}

// TestListCommentsFollowsPagination: a comment on page 2 must be returned.
func TestListCommentsFollowsPagination(t *testing.T) {
	page1 := mustJSON(t, []map[string]interface{}{{"id": 1, "body": "one", "user": map[string]string{"login": "a"}}})
	page2 := mustJSON(t, []map[string]interface{}{{"id": 2, "body": "two", "user": map[string]string{"login": "b"}}})
	p, repo := paginationProvider(t, paginatedJSON(t, [][]byte{page1, page2}))

	comments, err := p.ListComments(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2 (page-2 comment was dropped — pagination not followed)", len(comments))
	}
}

// TestClaimWinnerSeesPage2Breadcrumb is the double-claim guard: the winning
// claim breadcrumb lands on page 2. Before #139, claimWinner read one page,
// found no breadcrumb, and returned (_, false) — the caller's "empty read = we
// win" branch, so a second racer double-claims. Now it must find it.
func TestClaimWinnerSeesPage2Breadcrumb(t *testing.T) {
	// 30 ordinary comments fill page 1; the claim is comment id 31 on page 2.
	var p1 []map[string]interface{}
	for i := 1; i <= 30; i++ {
		p1 = append(p1, map[string]interface{}{"id": i, "body": "chatter", "user": map[string]string{"login": "x"}})
	}
	page1 := mustJSON(t, p1)
	page2 := mustJSON(t, []map[string]interface{}{{"id": 31, "body": claimBreadcrumb("run-winner"), "user": map[string]string{"login": "goobers"}}})
	p, repo := paginationProvider(t, paginatedJSON(t, [][]byte{page1, page2}))

	winner, ok, err := p.claimWinner(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("claimWinner: %v", err)
	}
	if !ok || winner != "run-winner" {
		t.Fatalf("claimWinner = (%q, %v), want (\"run-winner\", true) — a page-2 breadcrumb was missed, which double-claims", winner, ok)
	}
}

// TestCombinedCheckStateSeesFailingRunOnPage2: a failing check-run beyond page
// 1 must make the whole state Failing (else the ci-gate passes a red PR).
func TestCombinedCheckStateSeesFailingRunOnPage2(t *testing.T) {
	mux := http.NewServeMux()
	// The legacy combined-status endpoint: one empty page.
	mux.HandleFunc("/repos/acme/app/commits/sha1/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(mustJSON(t, map[string]interface{}{"statuses": []interface{}{}}))
	})
	// check-runs: page 1 all passing, page 2 has a failure.
	page1 := mustJSON(t, map[string]interface{}{"check_runs": []map[string]interface{}{{"name": "build", "status": "completed", "conclusion": "success"}}})
	page2 := mustJSON(t, map[string]interface{}{"check_runs": []map[string]interface{}{{"name": "test", "status": "completed", "conclusion": "failure"}}})
	mux.Handle("/repos/acme/app/commits/sha1/check-runs", paginatedJSON(t, [][]byte{page1, page2}))
	p, repo := paginationProvider(t, mux)

	state, _, err := p.combinedCheckState(context.Background(), repo, "sha1")
	if err != nil {
		t.Fatalf("combinedCheckState: %v", err)
	}
	if state != CheckStateFailing {
		t.Fatalf("check state = %q, want %q — a failing check-run on page 2 was missed (red CI read as green)", state, CheckStateFailing)
	}
}

// TestReviewDecisionSeesChangesRequestedOnPage2: CHANGES_REQUESTED beyond page
// 1 must block (else a truly-rejected PR reads Approved).
func TestReviewDecisionSeesChangesRequestedOnPage2(t *testing.T) {
	page1 := mustJSON(t, []map[string]interface{}{{"state": "APPROVED", "user": map[string]string{"login": "a"}}})
	page2 := mustJSON(t, []map[string]interface{}{{"state": "CHANGES_REQUESTED", "user": map[string]string{"login": "b"}}})
	p, repo := paginationProvider(t, paginatedJSON(t, [][]byte{page1, page2}))

	decision, n, err := p.reviewDecision(context.Background(), repo, "11")
	if err != nil {
		t.Fatalf("reviewDecision: %v", err)
	}
	if decision != ReviewDecisionChangesRequested || n != 1 {
		t.Fatalf("decision = (%q, %d), want (changes_requested, 1) — a page-2 CHANGES_REQUESTED was missed", decision, n)
	}
}

// TestSendRetriesOnServerError: a transient 500 is retried and the request
// ultimately succeeds, rather than failing the caller on one blip (#139).
func TestSendRetriesOnServerError(t *testing.T) {
	var calls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(mustJSON(t, []map[string]interface{}{{"id": 1, "body": "ok", "user": map[string]string{"login": "a"}}}))
	})
	p, repo := paginationProvider(t, h)

	comments, err := p.ListComments(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("ListComments after a retried 500: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1", len(comments))
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server received %d calls, want 2 (one 500 + one success) — 5xx was not retried", got)
	}
}
