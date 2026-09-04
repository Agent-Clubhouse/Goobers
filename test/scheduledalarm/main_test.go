package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type fakeLister struct {
	runs map[string][]workflowRun
	err  error
}

func (f fakeLister) ScheduledRuns(_ context.Context, workflowFile string) ([]workflowRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.runs[workflowFile], nil
}

type fakeProvider struct {
	items   []providers.WorkItem
	labels  []providers.WorkItemLabel
	created []providers.CreateWorkItemRequest
	listed  []providers.ListWorkItemsRequest
	failOn  string
}

func (f *fakeProvider) EnsureWorkItemLabels(
	_ context.Context,
	_ providers.RepositoryRef,
	labels []providers.WorkItemLabel,
) (providers.EnsureWorkItemLabelsResult, error) {
	if f.failOn == "labels" {
		return providers.EnsureWorkItemLabelsResult{}, errors.New("label boom")
	}
	f.labels = append(f.labels, labels...)
	return providers.EnsureWorkItemLabelsResult{}, nil
}

func (f *fakeProvider) ListWorkItems(
	_ context.Context,
	request providers.ListWorkItemsRequest,
) ([]providers.WorkItem, error) {
	if f.failOn == "list" {
		return nil, errors.New("list boom")
	}
	f.listed = append(f.listed, request)
	return f.items, nil
}

func (f *fakeProvider) CreateWorkItem(
	_ context.Context,
	request providers.CreateWorkItemRequest,
) (providers.WorkItem, error) {
	if f.failOn == "create" {
		return providers.WorkItem{}, errors.New("create boom")
	}
	f.created = append(f.created, request)
	return providers.WorkItem{ID: "1", Body: request.Body}, nil
}

func failedRuns(count int) []workflowRun {
	base := time.Date(2026, 9, 1, 4, 17, 0, 0, time.UTC)
	runs := make([]workflowRun, 0, count)
	for index := range count {
		runs = append(runs, workflowRun{
			ID:         int64(100 + index),
			Conclusion: "failure",
			HTMLURL:    "https://github.com/acme/app/actions/runs/" + string(rune('0'+index)),
			CreatedAt:  base.AddDate(0, 0, -index),
		})
	}
	return runs
}

func testRepository() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
}

func TestConsecutiveFailuresCountsOnlyTheLeadingStreak(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 4, 17, 0, 0, time.UTC)
	runs := []workflowRun{
		// Deliberately out of order: the streak is defined by run time, not
		// by the order the API happened to return.
		{ID: 3, Conclusion: "failure", CreatedAt: base.AddDate(0, 0, -2)},
		{ID: 1, Conclusion: "timed_out", CreatedAt: base},
		{ID: 5, Conclusion: "failure", CreatedAt: base.AddDate(0, 0, -4)},
		{ID: 2, Conclusion: "failure", CreatedAt: base.AddDate(0, 0, -1)},
		{ID: 4, Conclusion: "success", CreatedAt: base.AddDate(0, 0, -3)},
	}
	found, ok := consecutiveFailures("stress.yml", runs, 3)
	if !ok || found.Length != 3 || found.Name != "stress" {
		t.Fatalf("consecutiveFailures() = %+v, %v", found, ok)
	}
	if ids := []int64{found.Runs[0].ID, found.Runs[1].ID, found.Runs[2].ID}; !slices.Equal(ids, []int64{1, 2, 3}) {
		t.Fatalf("streak run ids = %v, want newest first", ids)
	}
	if _, ok := consecutiveFailures("stress.yml", runs, 4); ok {
		t.Fatal("a three-run streak must not raise a four-run threshold")
	}
	for _, conclusion := range []string{"success", "cancelled", "skipped", ""} {
		interrupted := append([]workflowRun{{ID: 9, Conclusion: conclusion, CreatedAt: base.Add(time.Hour)}}, runs...)
		if _, ok := consecutiveFailures("stress.yml", interrupted, 3); ok {
			t.Fatalf("a %q run must end the streak", conclusion)
		}
	}
}

func TestRaiseFilesExactlyOneIssuePerStreak(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{}
	lister := fakeLister{runs: map[string][]workflowRun{
		"stress.yml":      failedRuns(4),
		"flake-watch.yml": failedRuns(2),
	}}
	workflows := []string{"flake-watch.yml", "stress.yml"}
	result, err := raise(context.Background(), provider, lister, testRepository(), workflows, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Streaks != 1 || result.Created != 1 || result.Known != 0 {
		t.Fatalf("result = %+v, want one streak filed once", result)
	}
	if len(provider.created) != 1 {
		t.Fatalf("created %d issues, want 1", len(provider.created))
	}
	created := provider.created[0]
	if !strings.Contains(created.Title, "stress") || !strings.Contains(created.Title, "4 runs in a row") {
		t.Fatalf("title = %q", created.Title)
	}
	if !strings.Contains(created.Body, markerPrefix+"stress.yml -->") {
		t.Fatalf("body carries no dedupe marker:\n%s", created.Body)
	}
	if !slices.Equal(created.Labels, []string{alarmLabel}) || created.RunID != "scheduled-streak-stress.yml" {
		t.Fatalf("labels/run id = %v / %q", created.Labels, created.RunID)
	}
	if len(provider.labels) != 1 || provider.labels[0].Name != alarmLabel {
		t.Fatalf("labels ensured = %+v", provider.labels)
	}
	if len(provider.listed) != 1 || provider.listed[0].State != "open" {
		t.Fatalf("list request = %+v, want an open-only query", provider.listed)
	}

	// The alarm issue it just filed is now open: a second pass must not re-file.
	provider.items = []providers.WorkItem{{ID: "1", State: "open", Body: created.Body}}
	provider.created = nil
	second, err := raise(context.Background(), provider, lister, testRepository(), workflows, 3)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created != 0 || second.Known != 1 || len(provider.created) != 0 {
		t.Fatalf("second pass = %+v, created %d issues; want no re-file", second, len(provider.created))
	}
}

func TestRaiseIgnoresAClosedAlarmIssue(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{items: []providers.WorkItem{
		{ID: "7", State: "closed", Body: markerPrefix + "stress.yml -->"},
	}}
	lister := fakeLister{runs: map[string][]workflowRun{"stress.yml": failedRuns(3)}}
	result, err := raise(context.Background(), provider, lister, testRepository(), []string{"stress.yml"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 {
		t.Fatalf("result = %+v, want a fresh alarm after the previous one was closed", result)
	}
}

func TestRaiseTouchesNoIssueWithoutAStreak(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{}
	lister := fakeLister{runs: map[string][]workflowRun{"stress.yml": failedRuns(2)}}
	result, err := raise(context.Background(), provider, lister, testRepository(), []string{"stress.yml"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Streaks != 0 || len(provider.labels) != 0 || len(provider.listed) != 0 {
		t.Fatalf("result = %+v, labels = %+v, lists = %+v; want no provider traffic", result, provider.labels, provider.listed)
	}
}

func TestRaiseReportsProviderFailures(t *testing.T) {
	t.Parallel()
	lister := fakeLister{runs: map[string][]workflowRun{"stress.yml": failedRuns(3)}}
	for _, failOn := range []string{"labels", "list", "create"} {
		provider := &fakeProvider{failOn: failOn}
		if _, err := raise(context.Background(), provider, lister, testRepository(), []string{"stress.yml"}, 3); err == nil {
			t.Fatalf("raise() succeeded with a failing %s call", failOn)
		}
	}
	broken := fakeLister{err: errors.New("api boom")}
	if _, err := raise(context.Background(), &fakeProvider{}, broken, testRepository(), []string{"stress.yml"}, 3); err == nil {
		t.Fatal("raise() succeeded with a failing run listing")
	}
}

func TestScheduledWorkflowsSelectsScheduledFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stress.yml", "name: Stress\non:\n  schedule:\n    - cron: \"17 4 * * *\"\n")
	write("nightly.yaml", "name: Nightly\non:\n  schedule:\n    - cron: \"0 0 * * *\"\n")
	write("ci.yml", "name: CI\non:\n  pull_request:\n")
	write("notes.md", "not a workflow\n")

	workflows, err := scheduledWorkflows(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(workflows, []string{"nightly.yaml", "stress.yml"}) {
		t.Fatalf("scheduledWorkflows() = %v", workflows)
	}
	if _, err := scheduledWorkflows(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("scheduledWorkflows() accepted a missing directory")
	}
	write("broken.yml", "name: [unterminated\n")
	if _, err := scheduledWorkflows(dir); err == nil {
		t.Fatal("scheduledWorkflows() accepted unparseable YAML")
	}
}

func TestRepositoryScheduledWorkflowsAreCovered(t *testing.T) {
	t.Parallel()
	workflows, err := scheduledWorkflows(filepath.Join("..", "..", ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(workflows, "stress.yml") {
		t.Fatalf("scheduled workflows = %v, want the nightly stress workflow", workflows)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "scheduled-failure-alarm.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"schedule:",
		"issues: write",
		"actions: read",
		"go run ./test/scheduledalarm",
		"concurrency:",
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("scheduled-failure-alarm.yml does not contain %q", want)
		}
	}
}

func TestGitHubRunListerReadsScheduledRuns(t *testing.T) {
	t.Parallel()
	var path, query, auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query, auth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"workflow_runs": []map[string]any{
				{"id": 1, "conclusion": "failure", "created_at": "2026-09-01T04:17:00Z"},
			},
		}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	lister := newRunLister("token", server.URL, "acme/app")
	runs, err := lister.ScheduledRuns(context.Background(), "stress.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Conclusion != "failure" {
		t.Fatalf("runs = %+v", runs)
	}
	if path != "/repos/acme/app/actions/workflows/stress.yml/runs" {
		t.Fatalf("path = %q", path)
	}
	for _, want := range []string{"event=schedule", "status=completed", "per_page=50"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q does not contain %q", query, want)
		}
	}
	if auth != "Bearer token" {
		t.Fatalf("authorization = %q", auth)
	}
}

func TestGitHubRunListerHandlesMissingAndFailingEndpoints(t *testing.T) {
	t.Parallel()
	status := http.StatusNotFound
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer server.Close()

	lister := newRunLister("token", server.URL+"/", "acme/app")
	runs, err := lister.ScheduledRuns(context.Background(), "never-ran.yml")
	if err != nil || runs != nil {
		t.Fatalf("ScheduledRuns(404) = %v, %v; want no runs and no error", runs, err)
	}
	status = http.StatusInternalServerError
	if _, err := lister.ScheduledRuns(context.Background(), "stress.yml"); err == nil {
		t.Fatal("ScheduledRuns() accepted a 500 response")
	}
}

func TestRunRequiresCredentialsAndValidArguments(t *testing.T) {
	t.Parallel()
	newProvider := func(string, string) alarmProvider { return &fakeProvider{} }
	newLister := func(string, string, string) runLister { return fakeLister{} }
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want int
	}{
		{name: "no token", args: nil, env: map[string]string{"GITHUB_REPOSITORY": "acme/app"}, want: 2},
		{
			name: "bad repository",
			env:  map[string]string{"GITHUB_TOKEN": "t", "GITHUB_REPOSITORY": "acme"},
			want: 2,
		},
		{
			name: "low threshold",
			args: []string{"-threshold", "1"},
			env:  map[string]string{"GITHUB_TOKEN": "t", "GITHUB_REPOSITORY": "acme/app"},
			want: 2,
		},
		{
			name: "positional argument",
			args: []string{"extra"},
			env:  map[string]string{"GITHUB_TOKEN": "t", "GITHUB_REPOSITORY": "acme/app"},
			want: 2,
		},
		{
			name: "missing workflow directory",
			args: []string{"-workflows", filepath.Join("testdata", "absent")},
			env:  map[string]string{"GITHUB_TOKEN": "t", "GITHUB_REPOSITORY": "acme/app"},
			want: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(name string) string { return test.env[name] }
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr, getenv, newProvider, newLister); code != test.want {
				t.Fatalf("run() = %d, want %d (stderr: %s)", code, test.want, stderr.String())
			}
		})
	}
}

func TestRunScansTheRepositoryWorkflows(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{}
	lister := fakeLister{runs: map[string][]workflowRun{"stress.yml": failedRuns(5)}}
	env := map[string]string{"GITHUB_TOKEN": "t", "GITHUB_REPOSITORY": "acme/app"}
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"-workflows", filepath.Join("..", "..", ".github", "workflows")},
		&stdout, &stderr,
		func(name string) string { return env[name] },
		func(string, string) alarmProvider { return provider },
		func(string, string, string) runLister { return lister },
	)
	if code != 0 {
		t.Fatalf("run() = %d (stderr: %s)", code, stderr.String())
	}
	if len(provider.created) != 1 || !strings.Contains(stdout.String(), "1 filed") {
		t.Fatalf("created = %d, stdout = %q", len(provider.created), stdout.String())
	}
}
