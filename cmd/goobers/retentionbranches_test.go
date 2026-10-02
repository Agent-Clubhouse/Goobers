package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func TestRetentionADOReadsCurrentPullRequestLabels(t *testing.T) {
	for _, label := range []string{"goobers:needs-human", "goobers:escalated", "goobers:blocked", "", "read-error"} {
		t.Run(label, func(t *testing.T) {
			var labelReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("retention attempted provider mutation: %s", r.Method)
					http.Error(w, "read only", http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/org/project/_apis/git/repositories/repo/pullrequests/17":
					_, _ = fmt.Fprint(w, `{"pullRequestId":17,"status":"active","repository":{"name":"repo","project":{"id":"project-id","name":"project"}}}`)
				case "/org/project/_apis/policy/evaluations":
					_, _ = fmt.Fprint(w, `{"value":[]}`)
				case "/org/project/_apis/git/repositories/repo/pullrequests/17/labels":
					labelReads.Add(1)
					if label == "read-error" {
						http.Error(w, "labels unavailable", http.StatusForbidden)
						return
					}
					_, _ = fmt.Fprintf(w, `{"value":[{"id":"label-id","name":%q}]}`, label)
				default:
					t.Errorf("unexpected retention read: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
			original := stageProviderFactories[providers.ProviderADO]
			t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = original })
			stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
				if !cfg.readOnly || cfg.cached || cfg.repo != repo {
					t.Fatalf("incorrect retention provider configuration: %+v", cfg)
				}
				provider := providers.NewADOProvider("org", "project", "test-token")
				provider.BaseURL = server.URL
				return provider, nil
			}
			parked, err := retentionItemParked(context.Background(), t.TempDir(), "17", recordedItemRepo{repo: repo, kind: itemKindPullRequest})
			if label == "read-error" {
				if err == nil {
					t.Fatal("unavailable labels authorized retention")
				}
			} else if err != nil || parked != (label != "") {
				t.Fatalf("parked=%v err=%v for label %q", parked, err, label)
			}
			if labelReads.Load() != 1 {
				t.Fatalf("current label reads=%d, want 1", labelReads.Load())
			}
		})
	}
}

func recordRetentionItem(t *testing.T, l instance.Layout, runID string) {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := recordItemRepository(log, runID, "17", itemKindIssue, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "recorded-owner", Name: "recorded-repo"}); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionBranchItemAuthority(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	createTerminalRun(t, l, "terminal")
	original := retentionItemParked
	t.Cleanup(func() { retentionItemParked = original })
	calls := 0
	parked := false
	var failure error
	retentionItemParked = func(_ context.Context, _ string, id string, entry recordedItemRepo) (bool, error) {
		calls++
		if id != "17" || entry.repo.Owner != "recorded-owner" {
			t.Fatalf("guessed routing: %s %+v", id, entry)
		}
		return parked, failure
	}
	allowed, err := retentionService(l).Allowed(context.Background(), l.RunsDir(), "terminal")
	if allowed || err == nil || calls != 0 {
		t.Fatalf("unknown ownership allowed: %v %v %d", allowed, err, calls)
	}
	// No active claim is created: released selections still protect the branch.
	recordRetentionItem(t, l, "terminal")
	allowed, err = retentionService(l).Allowed(context.Background(), l.RunsDir(), "terminal")
	if !allowed || err != nil {
		t.Fatalf("ordinary terminal: %v %v", allowed, err)
	}
	parked = true
	allowed, err = retentionService(l).Allowed(context.Background(), l.RunsDir(), "terminal")
	if allowed || err != nil || calls != 2 {
		t.Fatalf("parked or cached item: %v %v %d", allowed, err, calls)
	}
	parked = false
	failure = fmt.Errorf("read unavailable")
	allowed, err = retentionService(l).Allowed(context.Background(), l.RunsDir(), "terminal")
	if allowed || err == nil {
		t.Fatalf("read failure authorized: %v %v", allowed, err)
	}
}

func TestConfiguredTerminalBranchAgeDefaultGraceAndItemRevalidation(t *testing.T) {
	l := instance.NewLayout(initDeterministicDemo(t))
	ctx := context.Background()
	const runID = "aged-terminal"
	createTerminalRun(t, l, runID)
	recordRetentionItem(t, l, runID)
	manager, repo := commandWorktreeFixture(t, l)
	wt, err := manager.Create(ctx, worktree.CreateOptions{RepoURL: repo, RunID: runID, Branch: providers.BranchName("default-implement", runID), BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "unmerged.txt"), []byte("unmerged"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "unmerged.txt"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "unmerged branch"}} {
		cmd := testgit.Command(args...)
		cmd.Dir = wt.Path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	branch := wt.Branch
	if err := wt.Remove(ctx, worktree.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	repoDir, err := manager.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	setup := &schedulerSetup{Config: &instance.Config{}, LegacyWorktrees: manager}
	now := time.Now().Add(29 * 24 * time.Hour)
	oldClock := retentionNow
	retentionNow = func() time.Time { return now }
	t.Cleanup(func() { retentionNow = oldClock })
	oldLookup := retentionItemParked
	parked := false
	lookups := 0
	retentionItemParked = func(context.Context, string, string, recordedItemRepo) (bool, error) { lookups++; return parked, nil }
	t.Cleanup(func() { retentionItemParked = oldLookup })
	sweep := func() string {
		t.Helper()
		var out, errs bytes.Buffer
		if err := pruneConfiguredRetention(ctx, l, setup, &out, &errs); err != nil {
			t.Fatal(err)
		}
		if errs.Len() > 0 {
			t.Fatal(errs.String())
		}
		return out.String()
	}
	if got := sweep(); strings.Contains(got, "terminal-branch-age") || lookups != 0 {
		t.Fatalf("young branch checked: %s calls=%d", got, lookups)
	}
	now = now.Add(2 * 24 * time.Hour)
	if got := sweep(); !strings.Contains(got, "retention candidate rule=terminal-branch-age") {
		t.Fatalf("missing default/grace candidate: %s", got)
	}
	if !retentionBranchExists(repoDir, branch) {
		t.Fatal("grace deleted branch")
	}
	now = now.Add(8 * 24 * time.Hour)
	parked = true
	if got := sweep(); strings.Contains(got, "retention deleted") || !retentionBranchExists(repoDir, branch) {
		t.Fatalf("parked item deletion: %s", got)
	}
	parked = false
	if got := sweep(); !strings.Contains(got, "retention deleted rule=terminal-branch-age") || retentionBranchExists(repoDir, branch) {
		t.Fatalf("eligible branch retained: %s", got)
	}
}

func TestRetentionPreservesTerminalSiblingWithParkedItem(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	const owner = "owner"
	const sibling = "sibling"
	createTerminalRun(t, l, owner)
	createTerminalRun(t, l, sibling)
	recordRetentionItem(t, l, owner)
	recordRetentionItem(t, l, sibling)
	opts := worktree.RetentionOptions{Now: time.Now()}
	configureBranchRetention(context.Background(), l, map[string]string{"root": l.RunsDir()}, map[string]map[string][]string{"root": {"branch": {sibling}}}, &opts)
	old := retentionItemParked
	t.Cleanup(func() { retentionItemParked = old })
	calls := 0
	retentionItemParked = func(context.Context, string, string, recordedItemRepo) (bool, error) { calls++; return calls == 2, nil }
	if ok, err := opts.CanPruneBranch("root", owner, "branch"); ok || err != nil || calls != 2 {
		t.Fatalf("sibling item not protected: %v %v %d", ok, err, calls)
	}
}
