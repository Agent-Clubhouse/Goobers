package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingRecorder captures external-ref mutations for assertions.
type recordingRecorder struct {
	mu   sync.Mutex
	refs []ExternalRef
}

func (r *recordingRecorder) RecordExternalRef(_ context.Context, ref ExternalRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
}

func (r *recordingRecorder) last() (ExternalRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.refs) == 0 {
		return ExternalRef{}, false
	}
	return r.refs[len(r.refs)-1], true
}

// recordingObserver captures rate-limit events.
type recordingObserver struct {
	mu     sync.Mutex
	events []RateLimitEvent
}

func (o *recordingObserver) ObserveRateLimit(_ context.Context, ev RateLimitEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, ev)
}

func (o *recordingObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.events)
}

type staticTokenSource struct {
	token string
	calls int
}

func (s *staticTokenSource) Token(context.Context) (string, error) {
	s.calls++
	return s.token, nil
}

// issueMock is a minimal in-memory GitHub issue backend covering the endpoints the
// issue operations touch: read issue, list/post comments, add/remove labels, patch.
type issueMock struct {
	mu        sync.Mutex
	title     string
	body      string
	state     string
	labels    []string
	comments  []map[string]interface{}
	nextID    int64
	authSeen  string
	patchBody map[string]interface{}
}

func newIssueMock() *issueMock {
	return &issueMock{title: "Fix API", body: "do it", state: "open", labels: []string{"route/backend"}}
}

func (m *issueMock) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, m.comments)
		case http.MethodPost:
			var body map[string]string
			decodeJSON(t, r, &body)
			m.nextID++
			c := map[string]interface{}{"id": m.nextID, "body": body["body"], "user": map[string]string{"login": "goobers"}}
			m.comments = append(m.comments, c)
			writeJSON(t, w, c)
		default:
			t.Fatalf("unexpected comments method %s", r.Method)
		}
	})
	mux.HandleFunc("/repos/acme/app/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected labels method %s", r.Method)
		}
		var body struct {
			Labels []string `json:"labels"`
		}
		decodeJSON(t, r, &body)
		m.labels = uniqueStrings(append(m.labels, body.Labels...))
		writeJSON(t, w, labelObjects(m.labels))
	})
	// DELETE /labels/{name}
	mux.HandleFunc("/repos/acme/app/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method != http.MethodDelete {
			t.Fatalf("unexpected label-delete method %s", r.Method)
		}
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/app/issues/7/labels/")
		next := make([]string, 0, len(m.labels))
		found := false
		for _, l := range m.labels {
			if l == name {
				found = true
				continue
			}
			next = append(next, l)
		}
		if !found {
			http.Error(w, "label not found", http.StatusNotFound)
			return
		}
		m.labels = next
		writeJSON(t, w, labelObjects(m.labels))
	})
	// PATCH/DELETE /repos/acme/app/issues/comments/{id}: comment IDs are
	// repo-scoped, not nested under the issue number.
	mux.HandleFunc("/repos/acme/app/issues/comments/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/repos/acme/app/issues/comments/")
		for i, c := range m.comments {
			if fmt.Sprint(c["id"]) != id {
				continue
			}
			switch r.Method {
			case http.MethodPatch:
				var body map[string]string
				decodeJSON(t, r, &body)
				m.comments[i]["body"] = body["body"]
				writeJSON(t, w, m.comments[i])
				return
			case http.MethodDelete:
				m.comments = append(m.comments[:i], m.comments[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			default:
				t.Fatalf("unexpected comment mutation method %s", r.Method)
			}
		}
		http.Error(w, "comment not found", http.StatusNotFound)
	})
	mux.HandleFunc("/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.authSeen = r.Header.Get("Authorization")
		if r.Method == http.MethodPatch {
			var body map[string]interface{}
			decodeJSON(t, r, &body)
			m.patchBody = body
			if v, ok := body["title"].(string); ok {
				m.title = v
			}
			if v, ok := body["body"].(string); ok {
				m.body = v
			}
			if v, ok := body["state"].(string); ok {
				m.state = v
			}
		}
		writeJSON(t, w, m.issueJSON())
	})
	return mux
}

func (m *issueMock) issueJSON() map[string]interface{} {
	return map[string]interface{}{
		"id": 123, "number": 7, "title": m.title, "body": m.body, "state": m.state,
		"html_url": "https://github.com/acme/app/issues/7",
		"labels":   labelObjects(m.labels),
	}
}

func labelObjects(labels []string) []map[string]string {
	out := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, map[string]string{"name": l})
	}
	return out
}

func newIssueProvider(t *testing.T, m *issueMock, opts ...func(*GitHubProvider)) (*GitHubProvider, RepositoryRef) {
	t.Helper()
	srv := httptest.NewServer(m.handler(t))
	t.Cleanup(srv.Close)
	all := append([]func(*GitHubProvider){func(p *GitHubProvider) { p.BaseURL = srv.URL }}, opts...)
	return NewGitHubProvider("token", all...), RepositoryRef{Owner: "acme", Name: "app"}
}

func TestGitHubListComments(t *testing.T) {
	m := newIssueMock()
	created := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	m.comments = []map[string]interface{}{
		{"id": 1, "body": "first", "user": map[string]string{"login": "mona"}, "created_at": created, "html_url": "c1"},
	}
	p, repo := newIssueProvider(t, m)
	comments, err := p.ListComments(context.Background(), repo, "7")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 1 || comments[0].ID != "1" || comments[0].Author != "mona" || comments[0].Body != "first" {
		t.Fatalf("unexpected comments: %#v", comments)
	}
	if comments[0].CreatedAt == nil || !comments[0].CreatedAt.Equal(created) {
		t.Fatalf("expected created_at preserved, got %#v", comments[0].CreatedAt)
	}
}

func TestGitHubUpdateWorkItemEditsLabelsCloseComment(t *testing.T) {
	m := newIssueMock()
	rec := &recordingRecorder{}
	p, repo := newIssueProvider(t, m, WithMutationRecorder(rec))
	newTitle := "Fix API v2"
	item, err := p.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository:   repo,
		ID:           "7",
		Title:        &newTitle,
		AddLabels:    []string{LabelReady},
		RemoveLabels: []string{"route/backend"},
		State:        "closed",
		Comment:      "done and dusted",
	})
	if err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if item.Title != "Fix API v2" || item.State != "closed" {
		t.Fatalf("unexpected final item: %#v", item)
	}
	if !item.HasLabel(LabelReady) || item.HasLabel("route/backend") {
		t.Fatalf("labels not applied: %#v", item.Labels)
	}
	if got := len(m.comments); got != 1 {
		t.Fatalf("expected 1 comment posted, got %d", got)
	}
	// External-ref mutation recorded with before/after digests for each field.
	ref, ok := rec.last()
	if !ok {
		t.Fatal("expected an external-ref mutation to be recorded")
	}
	if ref.Ref != "acme/app#7" || ref.Operation != "close" {
		t.Fatalf("unexpected ref: %#v", ref)
	}
	for _, field := range []string{"title", "state", "labels", "comment"} {
		fd, ok := ref.Fields[field]
		if !ok {
			t.Fatalf("missing field digest %q in %#v", field, ref.Fields)
		}
		if field != "comment" && fd.Before == fd.After {
			t.Fatalf("field %q before==after digest (%s); expected change", field, fd.After)
		}
	}
	if ref.Fields["title"].Before != digestString("Fix API") || ref.Fields["title"].After != digestString("Fix API v2") {
		t.Fatalf("title digests wrong: %#v", ref.Fields["title"])
	}
}

func TestGitHubUpdateWorkItemNoChangeSkipsRecord(t *testing.T) {
	m := newIssueMock()
	rec := &recordingRecorder{}
	p, repo := newIssueProvider(t, m, WithMutationRecorder(rec))
	if _, err := p.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{Repository: repo, ID: "7"}); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if _, ok := rec.last(); ok {
		t.Fatal("no-op update should record no mutation")
	}
	if m.patchBody != nil {
		t.Fatalf("no-op update should not PATCH, got %#v", m.patchBody)
	}
}

// TestGitHubUpdateCommentEditsInPlace is #716's sticky-comment primitive: a
// caller with an existing comment's ID edits its body via PATCH rather than
// posting a new one — GitHub's comment-edit endpoint, not previously wired.
func TestGitHubUpdateCommentEditsInPlace(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)

	if err := p.postComment(context.Background(), repo, "7", "original body"); err != nil {
		t.Fatalf("postComment: %v", err)
	}
	m.mu.Lock()
	commentID := fmt.Sprint(m.comments[0]["id"])
	m.mu.Unlock()

	if err := p.UpdateComment(context.Background(), repo, commentID, "edited body"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.comments) != 1 {
		t.Fatalf("comments = %v, want exactly 1 — an edit must not create a second comment", m.comments)
	}
	if m.comments[0]["body"] != "edited body" {
		t.Fatalf("comment body = %v, want %q", m.comments[0]["body"], "edited body")
	}
}

func TestGitHubUpdateCommentRequiresID(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	if err := p.UpdateComment(context.Background(), repo, "", "body"); err == nil {
		t.Fatal("UpdateComment with an empty comment id: err = nil, want an error")
	}
}

func TestGitHubDeleteCommentIsIdempotent(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	if err := p.postComment(context.Background(), repo, "7", "obsolete"); err != nil {
		t.Fatalf("postComment: %v", err)
	}
	m.mu.Lock()
	commentID := fmt.Sprint(m.comments[0]["id"])
	m.mu.Unlock()

	if err := p.DeleteComment(context.Background(), repo, commentID); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if err := p.DeleteComment(context.Background(), repo, commentID); err != nil {
		t.Fatalf("DeleteComment retry: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.comments) != 0 {
		t.Fatalf("comments = %v, want none", m.comments)
	}
}

func TestGitHubDeleteCommentRequiresID(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	if err := p.DeleteComment(context.Background(), repo, ""); err == nil {
		t.Fatal("DeleteComment with an empty comment id: err = nil, want an error")
	}
}

func TestGitHubClaimSingleWinnerUnderConcurrency(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)

	var wg sync.WaitGroup
	results := make([]ClaimResult, 2)
	errs := make([]error, 2)
	runIDs := []string{"run-A", "run-B"}
	wg.Add(2)
	for i := range runIDs {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.ClaimWorkItem(context.Background(), ClaimWorkItemRequest{
				Repository: repo, ID: "7", RunID: runIDs[i],
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("claim %s: %v", runIDs[i], err)
		}
	}
	winners := 0
	var winner ClaimResult
	for _, r := range results {
		if r.Claimed {
			winners++
			winner = r
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one winner, got %d (%+v)", winners, results)
	}
	// Both runs must agree on who won.
	if results[0].ClaimedBy != results[1].ClaimedBy {
		t.Fatalf("runs disagree on winner: %q vs %q", results[0].ClaimedBy, results[1].ClaimedBy)
	}
	if winner.ClaimedBy != "run-A" && winner.ClaimedBy != "run-B" {
		t.Fatalf("winner not one of the racers: %q", winner.ClaimedBy)
	}
	// The winner's item carries the claimed label for human visibility. (A loser's
	// snapshot may predate the winner's label write, so we assert on the winner.)
	if !winner.Item.HasLabel(LabelClaimed) {
		t.Fatalf("claimed label not applied to winner: %#v", winner.Item.Labels)
	}
}

func TestGitHubClaimIdempotentAndAlreadyClaimed(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	ctx := context.Background()

	first, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !first.Claimed {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	// Re-claim by the same run is idempotent and must not post another breadcrumb.
	before := len(m.comments)
	again, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-A"})
	if err != nil || !again.Claimed || again.ClaimedBy != "run-A" {
		t.Fatalf("re-claim = %+v, %v", again, err)
	}
	if len(m.comments) != before {
		t.Fatalf("idempotent re-claim posted extra comment: %d -> %d", before, len(m.comments))
	}
	// A different run loses and does not post a breadcrumb (fast path).
	other, err := p.ClaimWorkItem(ctx, ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-B"})
	if err != nil {
		t.Fatalf("loser claim error: %v", err)
	}
	if other.Claimed || other.ClaimedBy != "run-A" {
		t.Fatalf("expected run-B to lose to run-A, got %+v", other)
	}
	if len(m.comments) != before {
		t.Fatalf("losing claim should not post a breadcrumb: %d -> %d", before, len(m.comments))
	}
}

func TestGitHubRateLimitBackoffAndTelemetry(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	reset := time.Now().Add(2 * time.Second).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset))
			http.Error(w, "secondary rate limit", http.StatusForbidden)
			return
		}
		writeJSON(t, w, map[string]interface{}{"id": 123, "number": 7, "title": "ok", "state": "open"})
	}))
	defer srv.Close()

	obs := &recordingObserver{}
	var waits []time.Duration
	p := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.BaseURL = srv.URL
	}, WithRateLimitObserver(obs))
	p.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	item, err := p.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err != nil {
		t.Fatalf("GetWorkItem under rate limit: %v", err)
	}
	if item.Title != "ok" {
		t.Fatalf("expected success after backoff, got %#v", item)
	}
	if obs.count() != 2 {
		t.Fatalf("expected 2 rate-limit telemetry events, got %d", obs.count())
	}
	if len(waits) != 2 {
		t.Fatalf("expected 2 backoff sleeps, got %v", waits)
	}
	for _, wt := range waits {
		if wt <= 0 {
			t.Fatalf("backoff wait not honored: %v", waits)
		}
	}
	if !obs.events[0].Secondary || obs.events[0].RetryAfter != time.Second {
		t.Fatalf("expected secondary rate-limit event honoring Retry-After, got %#v", obs.events[0])
	}
}

func TestGitHubRateLimitGivesUpAfterMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = srv.URL }, WithMaxRateLimitRetries(2))
	p.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := p.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err == nil {
		t.Fatal("expected error after exhausting rate-limit retries")
	}
	// The give-up error is typed (#614), never the generic non-2xx string.
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v (%T), want *RateLimitError", err, err)
	}
	if !rl.Secondary {
		t.Fatalf("Retry-After-driven limit should mark Secondary, got %+v", rl)
	}
}

func TestGitHubForbiddenResponsePreservesRetryGuidance(t *testing.T) {
	cases := []struct {
		name          string
		headers       map[string]string
		wantTransient bool
		wantCalls     int
		wantGuidance  string
	}{
		{
			name:          "secondary rate limit",
			headers:       map[string]string{"Retry-After": "1"},
			wantTransient: true,
			wantCalls:     2,
			wantGuidance:  `Retry-After="1"`,
		},
		{
			name: "primary rate limit",
			headers: map[string]string{
				"X-RateLimit-Remaining": "0",
				"X-RateLimit-Reset":     "1784210000",
			},
			wantTransient: true,
			wantCalls:     2,
			wantGuidance:  `X-RateLimit-Reset="1784210000"`,
		},
		{
			name:          "authorization",
			wantTransient: false,
			wantCalls:     1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				for key, value := range tc.headers {
					w.Header().Set(key, value)
				}
				http.Error(w, "forbidden", http.StatusForbidden)
			}))
			defer srv.Close()

			p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = srv.URL }, WithMaxRateLimitRetries(1))
			p.sleep = func(context.Context, time.Duration) error { return nil }
			_, err := p.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
			if err == nil {
				t.Fatal("expected forbidden response to fail")
			}
			if got := IsTransientError(err); got != tc.wantTransient {
				t.Fatalf("IsTransientError(%v) = %v, want %v", err, got, tc.wantTransient)
			}
			if calls != tc.wantCalls {
				t.Fatalf("provider calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantGuidance != "" && !strings.Contains(err.Error(), tc.wantGuidance) {
				t.Fatalf("error %q does not preserve %q", err, tc.wantGuidance)
			}
		})
	}
}

func TestGitHubListWorkItemsFiltersAndPagination(t *testing.T) {
	var gotQuery map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotQuery = map[string]string{
			"assignee": q.Get("assignee"), "since": q.Get("since"),
			"page": q.Get("page"), "per_page": q.Get("per_page"), "labels": q.Get("labels"),
		}
		writeJSON(t, w, []map[string]interface{}{
			{"id": 1, "number": 7, "title": "issue", "state": "open"},
			{"id": 2, "number": 8, "title": "a pr", "state": "open", "pull_request": map[string]string{"url": "pr-url"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = srv.URL })
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	items, err := p.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"},
		Labels:     []string{LabelReady}, Assignee: "mona", UpdatedSince: &since, Limit: 50, Page: 2,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	// The pull request entry must be excluded from a backlog issues query.
	if len(items) != 1 || items[0].ID != "7" {
		t.Fatalf("expected only the issue (PR excluded), got %#v", items)
	}
	if gotQuery["assignee"] != "mona" || gotQuery["since"] != "2026-07-01T00:00:00Z" ||
		gotQuery["page"] != "2" || gotQuery["per_page"] != "50" || gotQuery["labels"] != LabelReady {
		t.Fatalf("query params not wired: %#v", gotQuery)
	}
}

func TestGitHubTokenSourceResolvesPerRequest(t *testing.T) {
	m := newIssueMock()
	ts := &staticTokenSource{token: "dynamic-token"}
	p, repo := newIssueProvider(t, m, WithTokenSource(ts))
	if _, err := p.GetWorkItem(context.Background(), repo, "7"); err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	if ts.calls == 0 {
		t.Fatal("expected token source to be consulted")
	}
	if m.authSeen != "Bearer dynamic-token" {
		t.Fatalf("expected token-source token in Authorization header, got %q", m.authSeen)
	}
}
