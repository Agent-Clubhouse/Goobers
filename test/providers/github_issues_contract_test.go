// This file extends the contract suite with the GitHub issues-provider acceptance
// checks for issue #12 that are GitHub-specific (ADO reaches parity in V1, BL-033):
// the exactly-one-winner claim guarantee (WF-031), rate-limit backoff + telemetry,
// and an opt-in live smoke test behind an env flag. These run black-box against a
// mocked GitHub API (or, for the smoke test, the real API when explicitly enabled).
package providers_contract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

// githubIssueBackend is a tiny in-memory GitHub issue #7 backend with an atomic,
// monotonic comment-id allocator — enough to exercise claim races and rate limits.
type githubIssueBackend struct {
	mu       sync.Mutex
	labels   []string
	comments []map[string]interface{}
	nextID   int64
}

func (b *githubIssueBackend) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if r.Method == http.MethodPost {
			var body map[string]string
			_ = decodeBody(r, &body)
			b.nextID++
			c := map[string]interface{}{"id": b.nextID, "body": body["body"], "user": map[string]string{"login": "goobers"}}
			b.comments = append(b.comments, c)
			writeJSON(t, w, c)
			return
		}
		writeJSON(t, w, b.comments)
	})
	mux.HandleFunc("/repos/acme/app/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		var body struct {
			Labels []string `json:"labels"`
		}
		_ = decodeBody(r, &body)
		b.labels = append(b.labels, body.Labels...)
		writeJSON(t, w, labelObjs(b.labels))
	})
	mux.HandleFunc("/repos/acme/app/issues/7", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		writeJSON(t, w, map[string]interface{}{
			"id": 123, "number": 7, "title": "Fix API", "state": "open",
			"html_url": "https://github.com/acme/app/issues/7", "labels": labelObjs(b.labels),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func labelObjs(labels []string) []map[string]string {
	out := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, map[string]string{"name": l})
	}
	return out
}

func decodeBody(r *http.Request, out interface{}) error {
	return json.NewDecoder(r.Body).Decode(out)
}

// TestContract_GitHubClaimExactlyOneWinner: two concurrent claims on one issue must
// yield exactly one winner, and both attempts must agree on who it is (WF-031).
func TestContract_GitHubClaimExactlyOneWinner(t *testing.T) {
	backend := &githubIssueBackend{}
	srv := backend.server(t)
	p := providers.NewGitHubProvider("token", func(p *providers.GitHubProvider) { p.BaseURL = srv.URL })
	repo := providers.RepositoryRef{Owner: "acme", Name: "app"}

	var wg sync.WaitGroup
	results := make([]providers.ClaimResult, 2)
	errs := make([]error, 2)
	ids := []string{"run-A", "run-B"}
	wg.Add(2)
	for i := range ids {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.ClaimWorkItem(context.Background(), providers.ClaimWorkItemRequest{
				Repository: repo, ID: "7", RunID: ids[i],
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("claim %s: %v", ids[i], err)
		}
	}
	winners := 0
	for _, r := range results {
		if r.Claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one winner, got %d: %+v", winners, results)
	}
	if results[0].ClaimedBy != results[1].ClaimedBy || results[0].ClaimedBy == "" {
		t.Fatalf("attempts disagree on winner: %q vs %q", results[0].ClaimedBy, results[1].ClaimedBy)
	}
}

// rateLimitObserver records rate-limit telemetry events.
type rateLimitObserver struct {
	mu     sync.Mutex
	events []providers.RateLimitEvent
}

func (o *rateLimitObserver) ObserveRateLimit(_ context.Context, ev providers.RateLimitEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, ev)
}

// TestContract_GitHubRateLimitBackoff: a mocked 403 secondary-rate-limit response
// must be backed off and retried to success, and must emit a telemetry event.
func TestContract_GitHubRateLimitBackoff(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, "You have exceeded a secondary rate limit", http.StatusForbidden)
			return
		}
		writeJSON(t, w, map[string]interface{}{"id": 123, "number": 7, "title": "ok", "state": "open"})
	}))
	defer srv.Close()

	obs := &rateLimitObserver{}
	p := providers.NewGitHubProvider("token",
		func(p *providers.GitHubProvider) { p.BaseURL = srv.URL },
		providers.WithRateLimitObserver(obs),
	)
	start := time.Now()
	item, err := p.GetWorkItem(context.Background(), providers.RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err != nil {
		t.Fatalf("GetWorkItem under rate limit: %v", err)
	}
	if item.Title != "ok" {
		t.Fatalf("expected success after backoff, got %#v", item)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.events) != 1 {
		t.Fatalf("expected 1 rate-limit telemetry event, got %d", len(obs.events))
	}
	if obs.events[0].Status != http.StatusForbidden || !obs.events[0].Secondary {
		t.Fatalf("unexpected rate-limit event: %#v", obs.events[0])
	}
	// The Retry-After of 1s must actually be honored (backoff was not skipped).
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("backoff not honored; call returned in %v", elapsed)
	}
}

// TestContract_GitHubLiveSmoke is an opt-in read-only smoke test against the real
// GitHub API. It runs only when GOOBERS_GITHUB_LIVE_SMOKE=1 and a token + repo are
// provided, so CI (which mocks the API) always skips it.
func TestContract_GitHubLiveSmoke(t *testing.T) {
	if os.Getenv("GOOBERS_GITHUB_LIVE_SMOKE") != "1" {
		t.Skip("set GOOBERS_GITHUB_LIVE_SMOKE=1 (plus token + repo) to run the live smoke test")
	}
	token := firstNonEmpty(os.Getenv("GOOBERS_GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		t.Skip("live smoke test needs GOOBERS_GITHUB_TOKEN or GITHUB_TOKEN")
	}
	repoSpec := os.Getenv("GOOBERS_GITHUB_SMOKE_REPO") // "owner/name"
	owner, name, ok := strings.Cut(repoSpec, "/")
	if !ok {
		t.Skip("live smoke test needs GOOBERS_GITHUB_SMOKE_REPO in owner/name form")
	}
	p := providers.NewGitHubProvider(token)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items, err := p.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: providers.RepositoryRef{Owner: owner, Name: name},
		State:      "open", Limit: 50,
	})
	if err != nil {
		t.Fatalf("live ListWorkItems: %v", err)
	}
	// Every returned item must be an issue, never a pull request (PRs are excluded).
	for _, it := range items {
		if it.Type != "issue" {
			t.Fatalf("live query returned a non-issue item: %#v", it)
		}
	}
	t.Logf("live smoke ok: %s/%s returned %d open issue(s)", owner, name, len(items))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
