package providerfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestCheckContractReplaysRecordedRequests(t *testing.T) {
	t.Parallel()
	if err := CheckContract(context.Background(), validFixture()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckContractClassifiesAssertionFailure(t *testing.T) {
	t.Parallel()
	fixture := cloneFixture(t, validFixture())
	fixture.Exchanges[1].Response.Body = json.RawMessage(`{
		"id": 0,
		"number": 7,
		"title": "",
		"state": "open",
		"html_url": "https://github.com/fixture-owner/fixture-repo/issues/7",
		"created_at": "2000-01-01T00:00:00Z",
		"updated_at": "2000-01-01T00:00:00Z"
	}`)
	err := CheckContract(context.Background(), fixture)
	if !errors.Is(err, ErrContractAssertion) {
		t.Fatalf("CheckContract() error = %v, want ErrContractAssertion", err)
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
		"actions/upload-artifact@v7",
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
