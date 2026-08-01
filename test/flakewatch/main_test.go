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
	knownText := "WARNING: DATA RACE"
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
	mux.HandleFunc("/repos/acme/app/actions/runs/99/jobs", jsonHandler(map[string]any{
		"jobs": []workflowJob{{ID: 103, Conclusion: "failure"}},
	}))
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

func TestFailuresUsesRunJobsAndIgnoresCheckSummaryForFingerprint(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/actions/runs/99/jobs", paginatedHandler(
		map[string]any{"jobs": []workflowJob{{ID: 200, Conclusion: "success"}}},
		map[string]any{"jobs": []workflowJob{{ID: 201, Conclusion: "failure"}}},
	))
	mux.HandleFunc("/repos/acme/app/check-runs/201/annotations", paginatedHandler(
		[]annotation{{Path: "internal/runner/run_test.go", Title: "TestResume", Message: "WARNING: DATA RACE"}},
		[]annotation{{Path: "internal/runner/run_test.go", Message: "diagnostic without a test name"}},
	))
	mux.HandleFunc("/repos/acme/app/commits/shared-sha/check-runs", jsonHandler(map[string]any{
		"check_runs": []checkRun{{
			ID: 999, Conclusion: "failure",
			Output: struct {
				Title   string `json:"title"`
				Summary string `json:"summary"`
			}{Title: "unrelated latest check", Summary: "TestWrong test timed out"},
		}},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := &githubClient{base: server.URL, repository: "acme/app", token: "test", http: server.Client()}

	got, err := client.failures(context.Background(), source{
		SHA: "shared-sha", RunID: 99, URL: "https://github.test/actions/runs/99",
	}, time.Now())
	if err != nil {
		t.Fatalf("failures: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("failures = %+v, want one run-specific failure", got)
	}
	text := "WARNING: DATA RACE"
	want := flake.Fingerprint("./internal/runner", "TestResume", flake.NormalizeSignature(text))
	if got[0].Fingerprint != want {
		t.Fatalf("fingerprint = %q, want %q", got[0].Fingerprint, want)
	}
}

func TestCheckSummaryDoesNotChangeAnnotationFingerprint(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/commits/first/check-runs", jsonHandler(map[string]any{
		"check_runs": []checkRun{checkFixtureWithSummary(301, "first changing summary")},
	}))
	mux.HandleFunc("/repos/acme/app/commits/second/check-runs", jsonHandler(map[string]any{
		"check_runs": []checkRun{checkFixtureWithSummary(302, "completely different summary")},
	}))
	diagnostic := []annotation{{
		Path: "internal/runner/run_test.go", Title: "TestResume", Message: "WARNING: DATA RACE",
	}}
	mux.HandleFunc("/repos/acme/app/check-runs/301/annotations", jsonHandler(diagnostic))
	mux.HandleFunc("/repos/acme/app/check-runs/302/annotations", jsonHandler(diagnostic))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := &githubClient{base: server.URL, repository: "acme/app", token: "test", http: server.Client()}

	first, err := client.failures(context.Background(), source{SHA: "first"}, time.Now())
	if err != nil {
		t.Fatalf("first failures: %v", err)
	}
	second, err := client.failures(context.Background(), source{SHA: "second"}, time.Now())
	if err != nil {
		t.Fatalf("second failures: %v", err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Fingerprint != second[0].Fingerprint {
		t.Fatalf("fingerprints changed with check summary: first=%+v second=%+v", first, second)
	}
}

func TestLedgerPaginatesIssues(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", paginatedHandler(
		[]ledgerIssue{{Number: 1, Body: "<!-- goobers-flake-fingerprint:" + strings.Repeat("a", 64) + " -->"}},
		[]ledgerIssue{{Number: 2, Body: "<!-- goobers-flake-fingerprint:" + strings.Repeat("b", 64) + " -->"}},
	))
	server := httptest.NewServer(mux)
	defer server.Close()

	entries, err := (&githubClient{
		base: server.URL, repository: "acme/app", token: "test", http: server.Client(),
	}).ledger(context.Background())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(entries) != 2 || entries[1].Issue != 2 {
		t.Fatalf("entries = %+v, want both pages", entries)
	}
}

func TestSourcesPaginatesRunsAndPreservesHistoricalRunIDs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", jsonHandler([]pullRequest{
		pullFixture(7, "shared-sha", "https://github.test/acme/app/pull/7"),
	}))
	mux.HandleFunc("/repos/acme/app/pulls/7/files", paginatedHandler(
		[]map[string]string{{"filename": "README.md"}},
		[]map[string]string{{"filename": "docs/design.md"}},
	))
	mux.HandleFunc("/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		headSHA := r.URL.Query().Get("head_sha")
		if page == "" {
			next := "http://" + r.Host + r.URL.Path + "?" + r.URL.RawQuery + "&page=2"
			w.Header().Set("Link", "<"+next+`>; rel="next"`)
		}
		var runs []workflowRun
		switch {
		case headSHA != "" && page == "":
			runs = []workflowRun{{ID: 71, HeadSHA: headSHA, HTMLURL: "run-71", CreatedAt: now}}
		case headSHA != "":
			runs = []workflowRun{{ID: 72, HeadSHA: headSHA, HTMLURL: "run-72", CreatedAt: now}}
		case page == "":
			runs = []workflowRun{{ID: 81, HeadSHA: "main-a", HTMLURL: "run-81", CreatedAt: now}}
		default:
			runs = []workflowRun{{ID: 82, HeadSHA: "main-b", HTMLURL: "run-82", CreatedAt: now}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	sources, err := (&githubClient{
		base: server.URL, repository: "acme/app", branch: "main", token: "test", http: server.Client(),
	}).sources(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	var runIDs []int64
	for _, source := range sources {
		if source.RunID != 0 {
			runIDs = append(runIDs, source.RunID)
		}
		if source.PullRequest == 7 && !source.ChangedFiles["docs/design.md"] {
			t.Fatal("paginated PR file was omitted")
		}
	}
	if fmt.Sprint(runIDs) != "[71 72 81 82]" {
		t.Fatalf("run IDs = %v, want all paginated historical runs", runIDs)
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

func checkFixtureWithSummary(id int64, summary string) checkRun {
	check := checkRun{ID: id, Name: "unit", Conclusion: "failure"}
	check.Output.Title = "changing title"
	check.Output.Summary = summary
	return check
}

func jsonHandler(value any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			panic(err)
		}
	}
}

func paginatedHandler(first, second any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value := first
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+`?page=2>; rel="next"`)
		} else {
			value = second
		}
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
