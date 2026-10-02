package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/apireadcache"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

// These tests exercise internal/apireadcache through package-main consumers
// (the backlog scan window and the pr-select / gather-sibling-context
// commands), so they stay with cmd/goobers rather than moving with the cache's
// own unit tests.

func TestBacklogQueryListWorkItemsRefreshesWeakETag(t *testing.T) {
	const (
		readyLabel = "goobers:ready"
		weakETag   = `W/"labels"`
		firstBody  = `[
			{"id":1,"number":1,"title":"ready first","state":"open","labels":[{"name":"goobers:ready"}]}
		]`
		secondBody = `[
			{"id":1,"number":1,"title":"ready first","state":"open","labels":[{"name":"goobers:ready"}]},
			{"id":2,"number":2,"title":"newly ready","state":"open","labels":[{"name":"goobers:ready"}]}
		]`
	)

	var (
		newlyLabeled    atomic.Bool
		requests        atomic.Int32
		weakConditional atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.URL.Query().Get("labels"); got != readyLabel {
			t.Errorf("labels query = %q, want %q", got, readyLabel)
		}
		if r.Header.Get("If-None-Match") == weakETag {
			weakConditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", weakETag)
		if newlyLabeled.Load() {
			_, _ = io.WriteString(w, secondBody)
			return
		}
		_, _ = io.WriteString(w, firstBody)
	}))
	defer srv.Close()

	dir := t.TempDir()
	newProvider := func(snapshotID string) *providers.GitHubProvider {
		return providers.NewGitHubProvider("tok",
			apireadcache.Option(dir, snapshotID),
			func(p *providers.GitHubProvider) { p.BaseURL = srv.URL },
		)
	}
	list := func(snapshotID string) []providers.WorkItem {
		t.Helper()
		items, _, err := listBacklogScanWindow(
			context.Background(),
			newProvider(snapshotID),
			providers.RepositoryRef{Owner: "acme", Name: "app"},
			[]string{readyLabel},
			nil,
			"",
			nil,
			backlogScanPageSize,
			backlogScanCursor{},
			false,
		)
		if err != nil {
			t.Fatalf("list backlog scan window: %v", err)
		}
		return items
	}

	if items := list("tick-1"); len(items) != 1 || items[0].ID != "1" {
		t.Fatalf("first backlog tick = %+v, want issue 1", items)
	}
	newlyLabeled.Store(true)
	if items := list("tick-2"); len(items) != 2 || items[1].ID != "2" {
		t.Fatalf("next backlog tick = %+v, want newly labeled issue 2", items)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("provider requests = %d, want one full read per tick", got)
	}
	if got := weakConditional.Load(); got != 0 {
		t.Fatalf("weak conditional requests = %d, want 0", got)
	}
}

func TestPRSelectAndSiblingContextShareProductionListSnapshot(t *testing.T) {
	const selected = 10
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(selected, "Selected PR")
	server.addOpenPR(selected, "goobers/implementation/run-10", "main", "head-10", "base-10", false, nil, []fakePRFile{
		{path: "cmd/goobers/main.go", status: "modified"},
	})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv(providersnapshot.EnvVar, "tick-1")

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := server.pullListRequestCount(); got != 1 {
		t.Fatalf("production pr-select to sibling-context list requests = %d, want 1", got)
	}
}
