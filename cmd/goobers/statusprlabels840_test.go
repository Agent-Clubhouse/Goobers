package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestStatusPullRequestLabelCountsDispatchToGitea(t *testing.T) {
	const token = "status-gitea-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/gerty/goobers-hew/pulls" {
			t.Fatalf("path = %q, want Gitea pulls endpoint", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token "+token {
			t.Fatalf("Authorization = %q, want Gitea token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
  {"number":1,"state":"open","labels":[{"name":"goobers:blocked-on-sibling"}],"head":{"ref":"goobers/one","sha":"head-1"},"base":{"ref":"main","sha":"base"}},
  {"number":2,"state":"open","labels":[{"name":"goobers:merge-escalated"}],"head":{"ref":"goobers/two","sha":"head-2"},"base":{"ref":"main","sha":"base"}}
]`))
	}))
	defer server.Close()
	t.Setenv("GOOBERS_STATUS_GITEA_TOKEN", token)

	counts, err := queryStatusPRLabelCounts(context.Background(), &instance.Config{
		Repos: []instance.RepoRef{{
			Provider: "gitea",
			BaseURL:  server.URL,
			Owner:    "gerty",
			Name:     "goobers-hew",
			Token:    instance.TokenRef{Env: "GOOBERS_STATUS_GITEA_TOKEN"},
		}},
	})
	if err != nil {
		t.Fatalf("queryStatusPRLabelCounts: %v", err)
	}
	if counts.blockedOnSibling != 1 || counts.mergeEscalated != 1 {
		t.Fatalf("counts = %+v, want one blocked and one escalated Gitea PR", counts)
	}
}

func TestStatusReportsDistinctPullRequestLabelCounts(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "status-fixture-token")

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(1, "goobers/implementation/blocked-a", "main", "head-1", "base", false, []string{blockedOnSiblingLabel}, nil)
	server.addOpenPR(2, "goobers/implementation/blocked-b", "main", "head-2", "base", false, []string{blockedOnSiblingLabel}, nil)
	server.addOpenPR(3, "goobers/implementation/escalated", "main", "head-3", "base", false, []string{remediationEscalatedLabel}, nil)
	server.addOpenPR(4, "human/unparked", "main", "head-4", "base", false, nil, nil)

	previousLoader := loadStatusPRLabelCounts
	loadStatusPRLabelCounts = queryStatusPRLabelCounts
	t.Cleanup(func() { loadStatusPRLabelCounts = previousLoader })
	previousProvider := newStatusGitHubProvider
	newStatusGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newStatusGitHubProvider = previousProvider })

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	blockedLine := "Open PRs with goobers:blocked-on-sibling: 2"
	escalatedLine := "Open PRs with goobers:merge-escalated: 1"
	blockedAt := strings.Index(stdout, blockedLine)
	escalatedAt := strings.Index(stdout, escalatedLine)
	if blockedAt == -1 || escalatedAt == -1 || blockedAt > escalatedAt {
		t.Fatalf("stdout = %q, want distinct counts in blocked/escalated order", stdout)
	}
	_, checkStateRequests := server.requestCounts()
	if checkStateRequests != 0 {
		t.Fatalf("status resolved check state %d time(s), want label-only list query", checkStateRequests)
	}
}

func TestStatusKeepsLocalOutputWhenPullRequestCountsAreUnavailable(t *testing.T) {
	root := initDemo(t)
	previousLoader := loadStatusPRLabelCounts
	loadStatusPRLabelCounts = func(context.Context, *instance.Config) (statusPRLabelCounts, error) {
		return statusPRLabelCounts{}, errors.New("provider unavailable")
	}
	t.Cleanup(func() { loadStatusPRLabelCounts = previousLoader })

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"Open PR label counts unavailable: provider unavailable",
		"no runs found",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
}

func TestStatusPullRequestLabelCountsUseBoundedRefreshCadence(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	loads := 0
	cache := &statusPRLabelCountCache{
		load: func(context.Context, *instance.Config) (statusPRLabelCounts, error) {
			loads++
			return statusPRLabelCounts{blockedOnSibling: loads}, nil
		},
		now: func() time.Time { return now },
	}

	first, err := cache.Load(context.Background(), &instance.Config{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(localscheduler.DefaultOpenPRRefreshInterval - time.Second)
	cached, err := cache.Load(context.Background(), &instance.Config{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	refreshed, err := cache.Load(context.Background(), &instance.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if loads != 2 || first.blockedOnSibling != 1 || cached.blockedOnSibling != 1 || refreshed.blockedOnSibling != 2 {
		t.Fatalf("loads = %d, counts = %d/%d/%d, want two loads with one cached result",
			loads, first.blockedOnSibling, cached.blockedOnSibling, refreshed.blockedOnSibling)
	}
}
