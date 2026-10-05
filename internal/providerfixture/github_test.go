package providerfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshNormalizesVolatileFields(t *testing.T) {
	t.Parallel()
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dedicated-token" {
			t.Errorf("Authorization header was not set from the dedicated token")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", 5000+round))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", 4999-round))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", 1900000000+round))
		switch r.URL.Path {
		case "/repos/acme/live/issues":
			round++
			if got := r.URL.Query().Encode(); got != "direction=asc&page=1&per_page=100&sort=created&state=open" {
				t.Errorf("list query = %q", got)
			}
			writeIssueJSON(t, w, []any{liveIssue(round)})
		case "/repos/acme/live/issues/7":
			writeIssueJSON(t, w, liveIssue(round))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := RefreshConfig{
		Repository: Repository{Owner: "acme", Name: "live"},
		Issue:      "7",
		Token:      "dedicated-token",
		BaseURL:    srv.URL,
	}
	first, err := Refresh(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Refresh(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstRaw, err := canonical(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := canonical(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstRaw, secondRaw) {
		t.Fatalf("volatile IDs, timestamps, or rate counters caused drift\nfirst:\n%s\nsecond:\n%s", firstRaw, secondRaw)
	}
	if bytes.Contains(firstRaw, []byte("dedicated-token")) {
		t.Fatal("fixture persisted its credential")
	}
	for _, want := range []string{
		`"owner": "fixture-owner"`,
		`"id": 0`,
		`"node_id": "NORMALIZED"`,
		`"created_at": "2000-01-01T00:00:00Z"`,
		`"X-RateLimit-Remaining": "0"`,
		`https://github.com/fixture-owner/fixture-repo/issues/7`,
	} {
		if !bytes.Contains(firstRaw, []byte(want)) {
			t.Errorf("normalized fixture does not contain %q:\n%s", want, firstRaw)
		}
	}
	if err := CheckContract(context.Background(), first); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  RefreshConfig
	}{
		{name: "repository", cfg: RefreshConfig{Issue: "7", Token: "token"}},
		{name: "issue", cfg: RefreshConfig{Repository: Repository{Owner: "acme", Name: "repo"}, Issue: "not-a-number", Token: "token"}},
		{name: "target", cfg: RefreshConfig{Repository: Repository{Owner: "acme", Name: "repo"}, Token: "token"}},
		{name: "multiple targets", cfg: RefreshConfig{Repository: Repository{Owner: "acme", Name: "repo"}, Issue: "7", PullRequest: "8", Token: "token"}},
		{name: "pull request", cfg: RefreshConfig{Repository: Repository{Owner: "acme", Name: "repo"}, PullRequest: "not-a-number", Token: "token"}},
		{name: "token", cfg: RefreshConfig{Repository: Repository{Owner: "acme", Name: "repo"}, Issue: "7"}},
		{name: "base URL", cfg: RefreshConfig{Repository: Repository{Owner: "acme", Name: "repo"}, Issue: "7", Token: "token", BaseURL: "://bad"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Refresh(context.Background(), tc.cfg); err == nil {
				t.Fatal("Refresh() succeeded with invalid configuration")
			}
		})
	}
}

func TestRefreshPreservesResponseErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		statusCode int
		body       []byte
		want       string
	}{
		{
			name:       "status",
			statusCode: http.StatusForbidden,
			body:       []byte(`{"message":"forbidden"}`),
			want:       `list-open-issues request returned status 403: {"message":"forbidden"}`,
		},
		{
			name:       "size",
			statusCode: http.StatusOK,
			body:       bytes.Repeat([]byte("x"), maxResponseBytes+1),
			want:       fmt.Sprintf("list-open-issues response exceeds %d bytes", maxResponseBytes),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Refresh(context.Background(), RefreshConfig{
				Repository: Repository{Owner: "acme", Name: "live"},
				Issue:      "7",
				Token:      "dedicated-token",
				Client: httpClientFunc(func(*http.Request) (*http.Response, error) {
					return fixtureHTTPResponse(tc.statusCode, tc.body), nil
				}),
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Refresh() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestRefreshRecordsPullRequestContractSet(t *testing.T) {
	t.Parallel()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/live/pulls":
			writeIssueJSON(t, w, []any{livePullRequest()})
		case "/repos/acme/live/pulls/8":
			writeIssueJSON(t, w, livePullRequest())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fixture, err := Refresh(context.Background(), RefreshConfig{
		Repository:  Repository{Owner: "acme", Name: "live"},
		PullRequest: "8",
		Token:       "dedicated-token",
		BaseURL:     srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.PullRequest != "8" || fixture.Issue != "" {
		t.Fatalf("fixture target = issue %q, pull request %q", fixture.Issue, fixture.PullRequest)
	}
	if got, want := strings.Join(paths, ","), "/repos/acme/live/pulls?per_page=100&state=open,/repos/acme/live/pulls/8"; got != want {
		t.Fatalf("request paths = %q, want %q", got, want)
	}
	if err := CheckContract(context.Background(), fixture); err != nil {
		t.Fatal(err)
	}
}

func TestReadWriteRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "fixture.json")
	want := validFixture()
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDrift(want, got); err != nil {
		t.Fatalf("round-tripped fixture drifted: %v", err)
	}
}

func TestExistingCanonicalFixtureBytesSurviveCheckCycle(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"github_contract.json", "github_pr_contract.json", "ado_contract.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "test", "providers", "testdata", name)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fixture, err := Read(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckContract(context.Background(), fixture); err != nil {
				t.Fatal(err)
			}
			afterPath := filepath.Join(t.TempDir(), name)
			if err := Write(afterPath, fixture); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(afterPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("canonical fixture bytes changed after check cycle\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestRefreshMatchesExistingCanonicalFixtureBytes(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"github_contract.json", "github_pr_contract.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "test", "providers", "testdata", name)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := Read(path)
			if err != nil {
				t.Fatal(err)
			}
			refreshed, err := Refresh(context.Background(), RefreshConfig{
				Repository:  baseline.Repository,
				Issue:       baseline.Issue,
				PullRequest: baseline.PullRequest,
				Token:       "dedicated-token",
				Client:      fixtureReplayClient(t, baseline),
			})
			if err != nil {
				t.Fatal(err)
			}
			after, err := canonical(refreshed)
			if err != nil {
				t.Fatal(err)
			}
			after = append(after, '\n')
			if !bytes.Equal(before, after) {
				t.Fatalf("canonical GitHub fixture bytes changed after refresh\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// TestRefreshRecordsPathsRelativeToBaseURLPrefix pins that a base URL with a
// path prefix (GitHub Enterprise Server's /api/v3) does not leak into the
// recorded exchange paths: they stay relative to the API root, as the request
// specs name them.
func TestRefreshRecordsPathsRelativeToBaseURLPrefix(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "test", "providers", "testdata", "github_contract.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	replay := fixtureReplayClient(t, baseline)
	prefixed := httpClientFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.Path, "/api/v3/") {
			return nil, fmt.Errorf("request %s did not use the base URL path prefix", req.URL.RequestURI())
		}
		stripped := req.Clone(req.Context())
		stripped.URL.Path = strings.TrimPrefix(req.URL.Path, "/api/v3")
		stripped.URL.RawPath = ""
		return replay.Do(stripped)
	})
	refreshed, err := Refresh(context.Background(), RefreshConfig{
		Repository: baseline.Repository,
		Issue:      baseline.Issue,
		Token:      "dedicated-token",
		BaseURL:    "https://ghes.invalid/api/v3/",
		Client:     prefixed,
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := canonical(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	after = append(after, '\n')
	if !bytes.Equal(before, after) {
		t.Fatalf("base URL path prefix changed recorded fixture bytes\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestCheckContractReplaysRecordedRequests(t *testing.T) {
	t.Parallel()
	if err := CheckContract(context.Background(), validFixture()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckContractPreservesIdentityAssertionError(t *testing.T) {
	t.Parallel()
	fixture := cloneFixture(t, validFixture())
	fixture.Exchanges[1].Response.Body = json.RawMessage(`{
		"id": 0,
		"number": 9,
		"title": "Stable fixture issue",
		"state": "open",
		"html_url": "https://github.com/fixture-owner/fixture-repo/issues/9",
		"created_at": "2000-01-01T00:00:00Z",
		"updated_at": "2000-01-01T00:00:00Z"
	}`)
	err := CheckContract(context.Background(), fixture)
	if !errors.Is(err, ErrContractAssertion) {
		t.Fatalf("CheckContract() error = %v, want ErrContractAssertion", err)
	}
	if got := errors.Unwrap(err); got != ErrContractAssertion {
		t.Fatalf("errors.Unwrap(CheckContract()) = %v, want ErrContractAssertion", got)
	}
	if got, want := err.Error(), "provider contract assertion failed: mapped item identity = issue/9, want issue/7"; got != want {
		t.Fatalf("CheckContract() error = %q, want %q", got, want)
	}
}

func TestCheckContractPreservesUnconsumedExchangeError(t *testing.T) {
	t.Parallel()
	fixture := cloneFixture(t, validFixture())
	fixture.Exchanges = append(fixture.Exchanges, Exchange{
		Name:   "unused",
		Method: http.MethodGet,
		Path:   "/unused",
		Response: FixtureResponse{
			Status: http.StatusOK,
			Body:   json.RawMessage(`{}`),
		},
	})
	err := CheckContract(context.Background(), fixture)
	if !errors.Is(err, ErrContractAssertion) {
		t.Fatalf("CheckContract() error = %v, want ErrContractAssertion", err)
	}
	if got, want := err.Error(), `provider contract assertion failed: fixture exchange "unused" was not consumed`; got != want {
		t.Fatalf("CheckContract() error = %q, want %q", got, want)
	}
}

func TestCheckDriftDistinguishesEquivalentAndMaterialFixtures(t *testing.T) {
	t.Parallel()
	baseline := validFixture()
	equivalent := cloneFixture(t, baseline)
	if err := CheckDrift(baseline, equivalent); err != nil {
		t.Fatalf("equivalent fixtures drifted: %v", err)
	}

	candidate := cloneFixture(t, baseline)
	candidate.Exchanges[1].Response.Body = json.RawMessage(strings.ReplaceAll(
		string(candidate.Exchanges[1].Response.Body),
		"Stable fixture issue",
		"Upstream renamed field",
	))
	err := CheckDrift(baseline, candidate)
	if !errors.Is(err, ErrFixtureDrift) {
		t.Fatalf("CheckDrift() error = %v, want ErrFixtureDrift", err)
	}
	if !strings.Contains(err.Error(), "baseline sha256:") || !strings.Contains(err.Error(), "candidate sha256:") {
		t.Fatalf("drift error does not identify both normalized inputs: %v", err)
	}
}

func TestProviderFixtureWorkflowIsInertAndSeparatesOutcomes(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "provider-fixture-drift.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)
	for _, want := range []string{
		"workflow_dispatch:",
		"Verify explicit provisioning",
		"Issue provider contract assertions",
		"Pull request provider contract assertions",
		"PROVIDER_FIXTURE_PR",
		"actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a # v7",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("provider fixture workflow does not contain %q", want)
		}
	}
	if got := strings.Count(workflow, "secrets.GH_READONLY_VALIDATION_PAT"); got != 3 {
		t.Errorf("provider fixture workflow contains %d GH_READONLY_VALIDATION_PAT references, want 3", got)
	}
	if strings.Contains(workflow, "secrets.PROVIDER_FIXTURE_TOKEN") {
		t.Fatal("provider fixture workflow references the obsolete PROVIDER_FIXTURE_TOKEN secret")
	}
	if strings.Contains(workflow, "pull_request:") {
		t.Fatal("provider fixture workflow must not run in pull-request CI")
	}
	for _, line := range strings.Split(workflow, "\n") {
		if strings.TrimSpace(line) == "schedule:" {
			t.Fatal("provider fixture schedule must remain disabled until explicit provisioning")
		}
	}
}

func liveIssue(round int) map[string]any {
	return map[string]any{
		"id":         1000 + round,
		"node_id":    fmt.Sprintf("node-%d", round),
		"number":     7,
		"title":      "Stable fixture issue",
		"body":       "Stable fixture body.",
		"state":      "open",
		"html_url":   "https://github.com/acme/live/issues/7",
		"created_at": fmt.Sprintf("2026-07-%02dT01:02:03Z", round),
		"updated_at": fmt.Sprintf("2026-07-%02dT04:05:06Z", round),
		"labels": []any{
			map[string]any{"id": 2000 + round, "node_id": fmt.Sprintf("label-%d", round), "name": "goobers:ready"},
		},
	}
}

func livePullRequest() map[string]any {
	return map[string]any{
		"id":         3008,
		"number":     8,
		"title":      "Stable fixture pull request",
		"body":       "Stable pull request body.",
		"state":      "open",
		"html_url":   "https://github.com/acme/live/pull/8",
		"draft":      false,
		"updated_at": "2026-07-01T04:05:06Z",
		"user":       map[string]any{"id": 4001, "login": "fixture-author"},
		"assignees": []any{
			map[string]any{"id": 4002, "login": "fixture-assignee"},
		},
		"requested_reviewers": []any{
			map[string]any{"id": 4003, "login": "fixture-reviewer"},
		},
		"head": map[string]any{"ref": "fixture-head", "sha": "head-sha"},
		"base": map[string]any{"ref": "main", "sha": "base-sha"},
	}
}

func writeIssueJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (f httpClientFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func fixtureHTTPResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func fixtureReplayClient(t *testing.T, fixture Fixture) HTTPClient {
	t.Helper()
	exchanges := append([]Exchange(nil), fixture.Exchanges...)
	return httpClientFunc(func(req *http.Request) (*http.Response, error) {
		if len(exchanges) == 0 {
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.RequestURI())
		}
		exchange := exchanges[0]
		exchanges = exchanges[1:]
		if req.Method != exchange.Method || req.URL.RequestURI() != exchange.Path {
			return nil, fmt.Errorf(
				"request = %s %s, want %s %s",
				req.Method,
				req.URL.RequestURI(),
				exchange.Method,
				exchange.Path,
			)
		}
		headers := make(http.Header, len(exchange.Response.Headers))
		for name, value := range exchange.Response.Headers {
			headers.Set(name, value)
		}
		return &http.Response{
			StatusCode: exchange.Response.Status,
			Header:     headers,
			Body:       io.NopCloser(bytes.NewReader(exchange.Response.Body)),
		}, nil
	})
}

func validFixture() Fixture {
	listBody := json.RawMessage(`[{
		"id": 0,
		"node_id": "NORMALIZED",
		"number": 7,
		"title": "Stable fixture issue",
		"body": "Stable fixture body.",
		"state": "open",
		"html_url": "https://github.com/fixture-owner/fixture-repo/issues/7",
		"labels": [{"id": 0, "node_id": "NORMALIZED", "name": "goobers:ready"}],
		"assignees": [{"id": 0, "node_id": "NORMALIZED", "login": "fixture-user"}],
		"created_at": "2000-01-01T00:00:00Z",
		"updated_at": "2000-01-01T00:00:00Z"
	}]`)
	getBody := json.RawMessage(`{
		"id": 0,
		"node_id": "NORMALIZED",
		"number": 7,
		"title": "Stable fixture issue",
		"body": "Stable fixture body.",
		"state": "open",
		"html_url": "https://github.com/fixture-owner/fixture-repo/issues/7",
		"labels": [{"id": 0, "node_id": "NORMALIZED", "name": "goobers:ready"}],
		"assignees": [{"id": 0, "node_id": "NORMALIZED", "login": "fixture-user"}],
		"created_at": "2000-01-01T00:00:00Z",
		"updated_at": "2000-01-01T00:00:00Z"
	}`)
	return Fixture{
		SchemaVersion: SchemaVersion,
		Provider:      "github",
		Repository:    Repository{Owner: normalizedOwner, Name: normalizedRepo},
		Issue:         "7",
		Exchanges: []Exchange{
			{
				Name:   "list-open-issues",
				Method: http.MethodGet,
				Path:   "/repos/fixture-owner/fixture-repo/issues?direction=asc&page=1&per_page=100&sort=created&state=open",
				Response: FixtureResponse{
					Status:  http.StatusOK,
					Headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "X-RateLimit-Remaining": "0"},
					Body:    listBody,
				},
			},
			{
				Name:   "get-issue",
				Method: http.MethodGet,
				Path:   "/repos/fixture-owner/fixture-repo/issues/7",
				Response: FixtureResponse{
					Status:  http.StatusOK,
					Headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "X-RateLimit-Remaining": "0"},
					Body:    getBody,
				},
			},
		},
	}
}

func cloneFixture(t *testing.T, fixture Fixture) Fixture {
	t.Helper()
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var clone Fixture
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}
