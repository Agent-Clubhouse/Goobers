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
	var comments []issueComment
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
		pullFixture(9, "known-regression-sha", "https://github.test/acme/app/pull/9"),
	}))
	mux.HandleFunc("/repos/acme/app/pulls/7/files", jsonHandler([]map[string]string{{"filename": "README.md"}}))
	mux.HandleFunc("/repos/acme/app/pulls/8/files", jsonHandler([]map[string]string{{"filename": "internal/cache/cache_test.go"}}))
	mux.HandleFunc("/repos/acme/app/pulls/9/files", jsonHandler([]map[string]string{{
		"filename": "internal/runner/run.go",
	}}))
	mux.HandleFunc("/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		runs := []workflowRun{}
		if r.URL.Query().Get("head_sha") == "" {
			runs = append(runs, workflowRun{
				ID: 99, HeadSHA: "branch-sha",
				HTMLURL: "https://github.test/acme/app/actions/runs/99", CreatedAt: now,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	})
	mux.HandleFunc("/repos/acme/app/commits/known-sha/check-runs", jsonHandler(checksFixture(101)))
	mux.HandleFunc("/repos/acme/app/commits/regression-sha/check-runs", jsonHandler(checksFixture(102)))
	mux.HandleFunc("/repos/acme/app/commits/branch-sha/check-runs", jsonHandler(checksFixture(103)))
	mux.HandleFunc("/repos/acme/app/commits/known-regression-sha/check-runs", jsonHandler(checksFixture(104)))
	mux.HandleFunc("/repos/acme/app/actions/runs/99/jobs", jsonHandler(map[string]any{
		"jobs": []workflowJob{{
			ID: 103, CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/103", Conclusion: "failure",
		}},
	}))
	mux.HandleFunc("/repos/acme/app/actions/jobs/103/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ordinary non-test job output")
	})
	mux.HandleFunc("/repos/acme/app/check-runs/101/annotations", jsonHandler([]annotation{{
		Path: "internal/runner/run_test.go", Title: "TestResume", Message: "WARNING: DATA RACE",
	}}))
	mux.HandleFunc("/repos/acme/app/check-runs/102/annotations", jsonHandler([]annotation{{
		Path: "internal/cache/cache_test.go", Title: "TestCache", Message: "test timed out waiting for cache",
	}}))
	mux.HandleFunc("/repos/acme/app/check-runs/103/annotations", jsonHandler([]annotation{
		{Path: "internal/queue/queue_test.go", Title: "TestQueue", Message: "deadline exceeded waiting for worker"},
		{
			Path: "cmd/goobers/worktreelifecycle_test.go", Title: "TestDaemonDrainMidAgenticStageFinalizesOwnedWorktrees",
			Message: `worktreelifecycle_test.go:105: state = "active", want "finalized"`,
		},
	}))
	mux.HandleFunc("/repos/acme/app/check-runs/104/annotations", jsonHandler([]annotation{{
		Path: "internal/runner/run_test.go", Title: "TestResume", Message: "WARNING: DATA RACE",
	}}))
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
	mux.HandleFunc("/repos/acme/app/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var comment issueComment
			if err := json.NewDecoder(r.Body).Decode(&comment); err != nil {
				t.Errorf("decode comment: %v", err)
			}
			comments = append(comments, comment)
			w.WriteHeader(http.StatusCreated)
			return
		}
		_ = json.NewEncoder(w).Encode(comments)
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
	if len(result.Novel) != 2 ||
		result.Novel[0].Test != "TestQueue" ||
		result.Novel[1].Test != "TestDaemonDrainMidAgenticStageFinalizesOwnedWorktrees" {
		t.Fatalf("novel = %+v, want timeout and deterministic-assert failures from default branch", result.Novel)
	}
	assertionText := `worktreelifecycle_test.go:105: state = "active", want "finalized"`
	assertionFingerprint := flake.Fingerprint(
		"./cmd/goobers",
		"TestDaemonDrainMidAgenticStageFinalizesOwnedWorktrees",
		flake.NormalizeSignature(assertionText),
	)
	if result.Novel[1].Fingerprint != assertionFingerprint {
		t.Fatalf("deterministic-assert fingerprint = %q, want %q", result.Novel[1].Fingerprint, assertionFingerprint)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dispatches) != 1 || dispatches[0]["event_type"] != "flake-fixer" {
		t.Fatalf("dispatches = %#v", dispatches)
	}
}

func TestScanLedgersCrossPackagePRFailureAbsentFromBase(t *testing.T) {
	// A novel failure in a package the PR didn't touch must still be
	// ledgered even when it doesn't also reproduce on the base SHA: a
	// nondeterministic flake commonly won't show up in the base's latest
	// checks, and requiring that reproduction incorrectly treats
	// non-reproduction as proof of regression, silently dropping real
	// novel flakes (see merge-review finding on #2349).
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", jsonHandler([]ledgerIssue{}))
	pull := pullFixture(7, "pr-sha", "pull-7")
	pull.Base.SHA = "base-sha"
	mux.HandleFunc("/repos/acme/app/pulls", jsonHandler([]pullRequest{pull}))
	mux.HandleFunc("/repos/acme/app/pulls/7/files", jsonHandler([]map[string]string{{
		"filename": "internal/storage/storage.go",
	}}))
	mux.HandleFunc("/repos/acme/app/actions/runs", jsonHandler(map[string]any{
		"workflow_runs": []workflowRun{},
	}))
	mux.HandleFunc("/repos/acme/app/commits/pr-sha/check-runs", jsonHandler(checksFixture(101)))
	mux.HandleFunc("/repos/acme/app/check-runs/101/annotations", jsonHandler([]annotation{{
		Path: "internal/cache/cache_test.go", Title: "TestCache",
		Message: "deadline exceeded waiting for storage",
	}}))
	mux.HandleFunc("/repos/acme/app/commits/base-sha/check-runs", jsonHandler(map[string]any{
		"check_runs": []checkRun{},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	result, err := scan(context.Background(), &githubClient{
		base: server.URL, repository: "acme/app", branch: "main", token: "test", http: server.Client(),
	}, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(result.Novel) != 1 || result.Novel[0].Test != "TestCache" {
		t.Fatalf("novel = %+v, want cross-package PR failure ledgered despite absence from base", result.Novel)
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
		map[string]any{"jobs": []workflowJob{{
			ID: 200, CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/300", Conclusion: "success",
		}}},
		map[string]any{"jobs": []workflowJob{{
			ID: 201, CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/301", Conclusion: "failure",
		}}},
	))
	mux.HandleFunc("/repos/acme/app/check-runs/301/annotations", paginatedHandler(
		[]annotation{{Path: "internal/runner/run_test.go", Title: "TestResume", Message: "WARNING: DATA RACE"}},
		[]annotation{{Path: "internal/runner/run_test.go", Message: "diagnostic without a test name"}},
	))
	mux.HandleFunc("/repos/acme/app/actions/jobs/201/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ordinary non-test job output")
	})
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

	scanned, err := client.failures(context.Background(), source{
		SHA: "shared-sha", RunID: 99, URL: "https://github.test/actions/runs/99",
	}, time.Now())
	if err != nil {
		t.Fatalf("failures: %v", err)
	}
	got := scanned.Failures
	if len(got) != 1 {
		t.Fatalf("failures = %+v, want one run-specific failure", got)
	}
	text := "WARNING: DATA RACE"
	want := flake.Fingerprint("./internal/runner", "TestResume", flake.NormalizeSignature(text))
	if got[0].Fingerprint != want {
		t.Fatalf("fingerprint = %q, want %q", got[0].Fingerprint, want)
	}
}

func TestFailuresParsesGoTestJobLogWithoutAnnotations(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/actions/runs/99/jobs", jsonHandler(map[string]any{
		"jobs": []workflowJob{{
			ID: 201, CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/301", Conclusion: "failure",
		}},
	}))
	mux.HandleFunc("/repos/acme/app/check-runs/301/annotations", jsonHandler([]annotation{}))
	mux.HandleFunc("/repos/acme/app/actions/jobs/201/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `2026-08-01T12:00:00.0000000Z === RUN   TestResume
	2026-08-01T12:00:00.0000000Z WARNING: DATA RACE
	2026-08-01T12:00:00.0000000Z Read at 0x00c000000000 by goroutine 19:
	2026-08-01T12:00:00.0000000Z   github.com/goobers/goobers/internal/runner.resume()
	2026-08-01T12:00:00.0000000Z       /home/runner/work/Goobers/internal/runner/run.go:42 +0x12
	2026-08-01T12:00:00.0000000Z --- FAIL: TestResume (0.02s)
	2026-08-01T12:00:00.0000000Z FAIL	github.com/goobers/goobers/internal/runner	1.234s
	`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	scanned, err := (&githubClient{
		base: server.URL, repository: "acme/app", token: "test", http: server.Client(),
	}).failures(context.Background(), source{SHA: "sha", RunID: 99, URL: "run-99"}, now)
	if err != nil {
		t.Fatalf("failures: %v", err)
	}
	got := scanned.Failures
	if len(got) != 1 {
		t.Fatalf("failures = %+v, want one log failure", got)
	}
	if got[0].Package != "./internal/runner" || got[0].Test != "TestResume" {
		t.Fatalf("failure identity = %s %s", got[0].Package, got[0].Test)
	}
	wantSignature := flake.NormalizeSignature(strings.Join([]string{
		"WARNING: DATA RACE",
		"Read at 0x00c000000000 by goroutine 19:",
		"github.com/goobers/goobers/internal/runner.resume()",
		"/home/runner/work/Goobers/internal/runner/run.go:42 +0x12",
	}, "\n"))
	if got[0].FailureSignature != wantSignature {
		t.Fatalf("signature = %q, want %q", got[0].FailureSignature, wantSignature)
	}
	if got[0].Occurrence == "" {
		t.Fatal("log failure has no occurrence identity")
	}
}

func TestParseGoTestFailuresSegmentsEachTest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	log := `setup output that must not affect either fingerprint
=== RUN   TestFirst
first assertion failed
--- FAIL: TestFirst (0.01s)
between-test output that must not affect either fingerprint
=== RUN   TestSecond
second assertion failed
--- FAIL: TestSecond (0.02s)
FAIL	github.com/goobers/goobers/internal/runner	0.03s
`

	got := parseGoTestFailures(log, "run-99", now)
	if len(got) != 2 {
		t.Fatalf("failures = %+v, want two", got)
	}
	for index, want := range []struct {
		test string
		text string
	}{
		{test: "TestFirst", text: "first assertion failed"},
		{test: "TestSecond", text: "second assertion failed"},
	} {
		if got[index].Test != want.test || got[index].FailureText != want.text {
			t.Fatalf("failure %d = %+v, want %s with isolated output %q", index, got[index], want.test, want.text)
		}
		signature := flake.NormalizeSignature(want.text)
		wantFingerprint := flake.Fingerprint("./internal/runner", want.test, signature)
		if got[index].Fingerprint != wantFingerprint {
			t.Fatalf("fingerprint %d = %q, want ledger fingerprint %q", index, got[index].Fingerprint, wantFingerprint)
		}
	}
}

func TestParseGoTestFailuresWithoutVerboseMarkers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	log := `--- FAIL: TestFirst (0.01s)
    first_test.go:12: first assertion failed
--- FAIL: TestSecond (0.02s)
    second_test.go:34: second assertion failed
FAIL	github.com/goobers/goobers/internal/runner	0.03s
`

	got := parseGoTestFailures(log, "run-99", now)
	if len(got) != 2 {
		t.Fatalf("failures = %+v, want two", got)
	}
	for index, want := range []struct {
		test string
		text string
	}{
		{test: "TestFirst", text: "first_test.go:12: first assertion failed"},
		{test: "TestSecond", text: "second_test.go:34: second assertion failed"},
	} {
		if got[index].Test != want.test || got[index].FailureText != want.text {
			t.Fatalf("failure %d = %+v, want %s with output %q", index, got[index], want.test, want.text)
		}
	}
}

func TestParseGoTestFailuresExtractsPackageTimeout(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	log := `=== RUN   TestBlocked
panic: test timed out after 10m0s
	running tests:
		TestBlocked (10m0s)

goroutine 1 [running]:
testing.(*M).startAlarm.func1()
	/usr/local/go/src/testing/testing.go:2484 +0x12
FAIL	github.com/goobers/goobers/internal/runner	600.005s
`

	got := parseGoTestFailures(log, "run-99", now)
	if len(got) != 1 {
		t.Fatalf("failures = %+v, want one package timeout", got)
	}
	if got[0].Package != "./internal/runner" || got[0].Test != "(package)" {
		t.Fatalf("failure identity = %s %s, want package timeout", got[0].Package, got[0].Test)
	}
	wantText := strings.Join([]string{
		"panic: test timed out after 10m0s",
		"running tests:",
		"TestBlocked (10m0s)",
		"",
		"goroutine 1 [running]:",
		"testing.(*M).startAlarm.func1()",
		"/usr/local/go/src/testing/testing.go:2484 +0x12",
	}, "\n")
	wantSignature := flake.NormalizeSignature(wantText)
	if got[0].FailureText != wantText || got[0].FailureSignature != wantSignature {
		t.Fatalf("timeout failure = %+v, want text %q and signature %q", got[0], wantText, wantSignature)
	}
	wantFingerprint := flake.Fingerprint("./internal/runner", "(package)", wantSignature)
	if got[0].Fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want ledger fingerprint %q", got[0].Fingerprint, wantFingerprint)
	}
}

func TestParseGoTestFailuresPreservesTimeoutAlongsideTestFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	log := `=== RUN   TestAssertion
assertion failed
--- FAIL: TestAssertion (0.01s)
=== RUN   TestBlocked
panic: test timed out after 10m0s
	running tests:
		TestBlocked (10m0s)
FAIL	github.com/goobers/goobers/internal/runner	600.005s
`

	got := parseGoTestFailures(log, "run-99", now)
	if len(got) != 2 {
		t.Fatalf("failures = %+v, want test failure and package timeout", got)
	}
	if got[0].Test != "TestAssertion" || got[1].Test != "(package)" {
		t.Fatalf("failure identities = %q, %q", got[0].Test, got[1].Test)
	}
	if !strings.Contains(got[1].FailureText, "test timed out") {
		t.Fatalf("timeout text = %q", got[1].FailureText)
	}
}

func TestScanDeduplicatesOverlappingSourcesAndPriorPolls(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	text := "WARNING: DATA RACE"
	signature := flake.NormalizeSignature(text)
	fingerprint := flake.Fingerprint("./internal/runner", "TestResume", signature)
	var mu sync.Mutex
	var dispatches int
	var comments []issueComment
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", jsonHandler([]ledgerIssue{{
		Number: 42,
		Body: fmt.Sprintf(
			"<!-- goobers-flake-fingerprint:%s -->\n- **Test:** `TestResume`\n- **Normalized signature:** `%s`\n",
			fingerprint,
			signature,
		),
	}}))
	mux.HandleFunc("/repos/acme/app/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var comment issueComment
			if err := json.NewDecoder(r.Body).Decode(&comment); err != nil {
				t.Errorf("decode comment: %v", err)
			}
			comments = append(comments, comment)
			w.WriteHeader(http.StatusCreated)
			return
		}
		_ = json.NewEncoder(w).Encode(comments)
	})
	mux.HandleFunc("/repos/acme/app/pulls", jsonHandler([]pullRequest{
		pullFixture(7, "shared-sha", "pull-7"),
	}))
	mux.HandleFunc("/repos/acme/app/pulls/7/files", jsonHandler([]map[string]string{{"filename": "README.md"}}))
	mux.HandleFunc("/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		runs := []workflowRun{}
		if r.URL.Query().Get("head_sha") != "" {
			runs = append(runs, workflowRun{ID: 99, HeadSHA: "shared-sha", HTMLURL: "run-99", CreatedAt: now})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	})
	mux.HandleFunc("/repos/acme/app/commits/shared-sha/check-runs", jsonHandler(checksFixture(201)))
	mux.HandleFunc("/repos/acme/app/actions/runs/99/jobs", jsonHandler(map[string]any{
		"jobs": []workflowJob{{
			ID: 201, CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/201", Conclusion: "failure",
		}},
	}))
	mux.HandleFunc("/repos/acme/app/check-runs/201/annotations", jsonHandler([]annotation{{
		Path: "internal/runner/run_test.go", Title: "TestResume", Message: text,
	}}))
	mux.HandleFunc("/repos/acme/app/actions/jobs/201/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ordinary non-test job output")
	})
	mux.HandleFunc("/repos/acme/app/dispatches", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		dispatches++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := &githubClient{
		base: server.URL, repository: "acme/app", branch: "main", token: "test", http: server.Client(),
	}

	first, err := scan(context.Background(), client, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	second, err := scan(context.Background(), client, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if first.KnownDispatched != 1 || second.KnownDispatched != 0 || dispatches != 1 || len(comments) != 1 {
		t.Fatalf(
			"first=%d second=%d dispatches=%d comments=%d, want 1/0/1/1",
			first.KnownDispatched, second.KnownDispatched, dispatches, len(comments),
		)
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

	firstScan, err := client.failures(context.Background(), source{SHA: "first"}, time.Now())
	if err != nil {
		t.Fatalf("first failures: %v", err)
	}
	secondScan, err := client.failures(context.Background(), source{SHA: "second"}, time.Now())
	if err != nil {
		t.Fatalf("second failures: %v", err)
	}
	first := firstScan.Failures
	second := secondScan.Failures
	if len(first) != 1 || len(second) != 1 || first[0].Fingerprint != second[0].Fingerprint {
		t.Fatalf("fingerprints changed with check summary: first=%+v second=%+v", first, second)
	}
}

func TestFailuresSkipsLoglessConclusionsAndRecordsMissingFailureLog(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/actions/runs/99/jobs", jsonHandler(map[string]any{
		"jobs": []workflowJob{
			{
				ID: 201, Name: "cancelled", Conclusion: "cancelled",
				CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/301",
			},
			{
				ID: 204, Name: "stale", Conclusion: "stale",
				CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/304",
			},
			{
				ID: 205, Name: "action required", Conclusion: "action_required",
				CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/305",
			},
			{
				ID: 202, Name: "failed without log", Conclusion: "failure", HTMLURL: "job-202",
				CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/302",
			},
			{
				ID: 203, Name: "timed out", Conclusion: "timed_out",
				CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/303",
			},
		},
	}))
	mux.HandleFunc("/repos/acme/app/check-runs/302/annotations", jsonHandler([]annotation{{
		Path: "internal/runner/run_test.go", Title: "TestResume", Message: "WARNING: DATA RACE",
	}}))
	mux.HandleFunc("/repos/acme/app/actions/jobs/202/logs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "BlobNotFound", http.StatusNotFound)
	})
	mux.HandleFunc("/repos/acme/app/check-runs/303/annotations", jsonHandler([]annotation{}))
	mux.HandleFunc("/repos/acme/app/actions/jobs/203/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `=== RUN   TestBlocked
panic: test timed out after 10m0s
--- FAIL: TestBlocked (10m0s)
FAIL	github.com/goobers/goobers/internal/runner	600.0s
`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	scanned, err := (&githubClient{
		base: server.URL, repository: "acme/app", token: "test", http: server.Client(),
	}).failures(context.Background(), source{SHA: "sha", RunID: 99, URL: "run-99"}, now)
	if err != nil {
		t.Fatalf("failures: %v", err)
	}
	if len(scanned.Failures) != 3 {
		t.Fatalf("failures = %+v, want annotation, timed-out test, and package timeout", scanned.Failures)
	}
	if len(scanned.LogOmissions) != 1 {
		t.Fatalf("log omissions = %+v, want failed job's missing log", scanned.LogOmissions)
	}
	omission := scanned.LogOmissions[0]
	if omission.JobID != 202 || omission.JobName != "failed without log" || omission.JobURL != "job-202" {
		t.Fatalf("log omission = %+v, want visible job diagnostics", omission)
	}
}

func TestFailuresDoesNotSuppressUnexpectedLogErrors(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/actions/runs/99/jobs", jsonHandler(map[string]any{
		"jobs": []workflowJob{{
			ID: 201, Conclusion: "failure",
			CheckRunURL: "https://api.github.test/repos/acme/app/check-runs/301",
		}},
	}))
	mux.HandleFunc("/repos/acme/app/check-runs/301/annotations", jsonHandler([]annotation{}))
	mux.HandleFunc("/repos/acme/app/actions/jobs/201/logs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream failure", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := (&githubClient{
		base: server.URL, repository: "acme/app", token: "test", http: server.Client(),
	}).failures(context.Background(), source{SHA: "sha", RunID: 99}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("failures error = %v, want unexpected log error", err)
	}
}

func TestLedgerPaginatesIssues(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/issues", paginatedHandler(
		[]ledgerIssue{{Number: 1, Body: "<!-- goobers-flake-fingerprint:" + strings.Repeat("a", 64) + " -->"}},
		[]ledgerIssue{{
			Number: 2,
			Body: "<!-- goobers-flake-fingerprint:" + strings.Repeat("b", 64) +
				" -->\n- **Package:** `./internal/runner`",
		}},
	))
	server := httptest.NewServer(mux)
	defer server.Close()

	entries, err := (&githubClient{
		base: server.URL, repository: "acme/app", token: "test", http: server.Client(),
	}).ledger(context.Background())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(entries) != 2 || entries[1].Issue != 2 || entries[1].Package != "./internal/runner" {
		t.Fatalf("entries = %+v, want both pages", entries)
	}
}

func TestLedgerSimilarityMatchIncludesPackage(t *testing.T) {
	t.Parallel()
	signature := "deadline exceeded waiting for worker"
	entry := ledgerEntry{
		Issue: 42, Package: "./internal/runner", Test: "TestResume", Signature: signature,
	}
	if _, ok := knownFailure([]ledgerEntry{entry}, failure{
		Package: "./internal/queue", Test: "TestResume", FailureSignature: signature,
	}); ok {
		t.Fatal("matched a similar failure from a different package")
	}
	if _, ok := knownFailure([]ledgerEntry{entry}, failure{
		Package: "./internal/runner", Test: "TestResume", FailureSignature: signature,
	}); !ok {
		t.Fatal("did not match a similar failure from the ledger package")
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

func TestSourceChecksExcludesChecksOutsideLookback(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/commits/pr-sha/check-runs", jsonHandler(map[string]any{
		"check_runs": []checkRun{
			{ID: 1, Conclusion: "failure", CompletedAt: since.Add(-time.Minute)},
			{ID: 2, Conclusion: "failure", CompletedAt: since},
		},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	checks, err := (&githubClient{
		base: server.URL, repository: "acme/app", token: "test", http: server.Client(),
	}).sourceChecks(context.Background(), source{SHA: "pr-sha", Since: since})
	if err != nil {
		t.Fatalf("sourceChecks: %v", err)
	}
	if len(checks) != 1 || checks[0].ID != 2 {
		t.Fatalf("checks = %+v, want only check completed within lookback", checks)
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
	return map[string]any{"check_runs": []checkRun{{
		ID: id, Name: "unit", Conclusion: "failure",
		CompletedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	}}}
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
