package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/flake"
)

func TestScanDispatchesKnownFilesNovelAndExcludesCorrelatedRegression(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	knownText := "TestResume\nWARNING: DATA RACE"
	knownSignature := flake.NormalizeSignature(knownText)
	knownFingerprint := flake.Fingerprint("./internal/runner", "TestResume", knownSignature)

	var mu sync.Mutex
	var dispatches []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", jsonHandler([]ledgerIssue{{
		Number: 42,
		Body: fmt.Sprintf(
			"<!-- goobers-flake-fingerprint:%s -->\n- **Test:** `TestResume`\n- **Normalized signature:** `%s`\n",
			knownFingerprint,
			knownSignature,
		),
	}}))
	mux.HandleFunc("/repos/acme/app/pulls", jsonHandler([]pullRequest{
		pullFixture(7, "known-sha", "https://github.test/acme/app/pull/7"),
		pullFixture(8, "regression-sha", "https://github.test/acme/app/pull/8"),
	}))
	mux.HandleFunc("/repos/acme/app/pulls/7/files", jsonHandler([]map[string]string{{"filename": "README.md"}}))
	mux.HandleFunc("/repos/acme/app/pulls/8/files", jsonHandler([]map[string]string{{"filename": "internal/cache/cache_test.go"}}))
	mux.HandleFunc("/repos/acme/app/actions/runs", jsonHandler(map[string]any{
		"workflow_runs": []workflowRun{{
			ID: 99, HeadSHA: "branch-sha", HTMLURL: "https://github.test/acme/app/actions/runs/99", CreatedAt: now,
		}},
	}))
	mux.HandleFunc("/repos/acme/app/commits/known-sha/check-runs", jsonHandler(checksFixture(101)))
	mux.HandleFunc("/repos/acme/app/commits/regression-sha/check-runs", jsonHandler(checksFixture(102)))
	mux.HandleFunc("/repos/acme/app/commits/branch-sha/check-runs", jsonHandler(checksFixture(103)))
	mux.HandleFunc("/repos/acme/app/check-runs/101/annotations", jsonHandler([]annotation{{
		Path: "internal/runner/run_test.go", Title: "TestResume", Message: "WARNING: DATA RACE",
	}}))
	mux.HandleFunc("/repos/acme/app/check-runs/102/annotations", jsonHandler([]annotation{{
		Path: "internal/cache/cache_test.go", Title: "TestCache", Message: "test timed out waiting for cache",
	}}))
	mux.HandleFunc("/repos/acme/app/check-runs/103/annotations", jsonHandler([]annotation{
		{Path: "internal/queue/queue_test.go", Title: "TestQueue", Message: "deadline exceeded waiting for worker"},
		{Path: "internal/queue/queue_test.go", Title: "TestCompile", Message: "unexpected compile output"},
	}))
	mux.HandleFunc("/repos/acme/app/dispatches", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode dispatch: %v", err)
		}
		mu.Lock()
		dispatches = append(dispatches, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := scan(context.Background(), &githubClient{
		base: server.URL, repository: "acme/app", token: "test", http: server.Client(),
	}, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result.KnownDispatched != 1 {
		t.Fatalf("known dispatched = %d, want 1", result.KnownDispatched)
	}
	if len(result.Novel) != 1 || result.Novel[0].Test != "TestQueue" {
		t.Fatalf("novel = %+v, want only default-branch TestQueue", result.Novel)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dispatches) != 1 || dispatches[0]["event_type"] != "flake-fixer" {
		t.Fatalf("dispatches = %#v", dispatches)
	}
}

func TestAnnotationPackageUsesStructuredModulePath(t *testing.T) {
	t.Parallel()
	got := annotationPackage("ignored.go", "github.com/goobers/goobers/internal/runner TestResume")
	if got != "./internal/runner" {
		t.Fatalf("annotationPackage = %q, want ./internal/runner", got)
	}
}

func TestCorrelatedWithPRIncludesChangedPackage(t *testing.T) {
	t.Parallel()
	failure := failure{Package: "./internal/cache", SourcePath: ""}
	if !correlatedWithPR(map[string]bool{"internal/cache/cache.go": true}, failure) {
		t.Fatal("failure in a changed package was not correlated with the PR")
	}
	if correlatedWithPR(map[string]bool{"internal/runner/run.go": true}, failure) {
		t.Fatal("failure was correlated with an unrelated package")
	}
}

func pullFixture(number int, sha, htmlURL string) pullRequest {
	pull := pullRequest{Number: number, HTMLURL: htmlURL}
	pull.Head.SHA = sha
	return pull
}

func checksFixture(id int64) map[string]any {
	return map[string]any{"check_runs": []checkRun{{ID: id, Name: "unit", Conclusion: "failure"}}}
}

func jsonHandler(value any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			panic(err)
		}
	}
}

func TestParseOptionsRejectsInvalidRepository(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	_, err := parseOptions(nil, &stderr, func(key string) string {
		if key == "GITHUB_REPOSITORY" {
			return "invalid"
		}
		return ""
	})
	if err == nil {
		t.Fatal("parseOptions accepted invalid repository")
	}
}
