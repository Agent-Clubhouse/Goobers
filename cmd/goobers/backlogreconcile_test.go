package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

func TestReconcileBacklogMetadataRepairsDriftAndLeavesCorrectLabelsUntouched(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "1000")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Orphaned claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	server.addComment(7, "goobers-claim: run=historical-run\n\nClaimed by an earlier run.")
	server.addIssue(8, "Live claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	server.addIssue(9, "Contradictory state", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.addIssue(10, "Empty tracker", "goobers:approved", providers.LabelReady, providers.LabelTracking)
	server.addIssue(11, "Native tracker", "goobers:approved", providers.LabelTracking, providers.LabelReady)
	server.addIssue(12, "Native child")
	server.addChild(11, 12)
	server.addIssue(13, "Checklist tracker", "goobers:approved", providers.LabelTracking)
	server.addIssue(14, "Checklist child")
	server.addIssue(15, "Completed tracker", "goobers:approved", providers.LabelReady, providers.LabelTracking)
	server.addIssue(16, "Completed child")
	server.addIssue(17, "Owned stale item", "goobers:approved", providers.LabelReady, providers.LabelStale)
	server.addIssue(18, "Active stale item", "goobers:approved", providers.LabelReady, providers.LabelStale)
	server.addIssue(19, "Still stale", "goobers:approved", providers.LabelReady, providers.LabelStale)
	server.addIssue(20, "Clean ready item", "goobers:approved", providers.LabelReady)
	server.addIssue(21, "Expired claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	server.addIssue(22, "Bot-active stale item", "goobers:approved", providers.LabelReady, providers.LabelStale)

	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	newStaleTerminalRun(t, layoutFor(root), "historical-run", "default-implement", journal.PhaseCompleted, "local-ci")
	server.mu.Lock()
	server.issues[13].body = "- [ ] #14"
	server.issues[15].body = "- [x] #16"
	server.issues[16].state = "closed"
	server.issues[17].assignee = "mona"
	server.issues[19].createdAt = now.Add(-100 * 24 * time.Hour)
	server.issues[22].createdAt = now.Add(-100 * 24 * time.Hour)
	server.mu.Unlock()
	server.addCommentAtAs(18, "mona", "Still wanted.", now.Add(-time.Hour))
	server.addCommentAtAsType(22, "dependabot[bot]", "Bot", "Automated update.", now.Add(-time.Hour))

	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(
		localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "7"},
		"historical-run", "default-implement", time.Hour,
	); err != nil || !ok {
		t.Fatalf("seed terminal orphan claim: ok=%v err=%v", ok, err)
	}
	claimKey := localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "8"}
	if ok, _, err := ledger.ClaimScoped(claimKey, "live-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}
	expiredLedger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now.Add(-2 * time.Hour) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	expiredKey := localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "21"}
	if ok, _, err := expiredLedger.ClaimScoped(expiredKey, "expired-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed expired claim: ok=%v err=%v", ok, err)
	}

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	provider := server.newGitHubProvider("token")
	reconciled, err := reconcileBacklogMetadata(context.Background(), layoutFor(root), provider, repo, "goobers:approved", defaultBacklogStalenessPolicy(), func() time.Time { return now })
	if err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	if reconciled != 8 {
		t.Fatalf("reconciliations = %d, want 8 actual corrections", reconciled)
	}

	assertFakeIssueLabels(t, server, 7, []string{"goobers:approved", providers.LabelReady}, []string{providers.LabelClaimed})
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelReady, providers.LabelClaimed}, nil)
	assertFakeIssueLabels(t, server, 9, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 10, []string{providers.LabelReady}, []string{providers.LabelTracking})
	assertFakeIssueLabels(t, server, 11, []string{providers.LabelTracking}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 13, []string{providers.LabelTracking}, nil)
	assertFakeIssueLabels(t, server, 15, []string{providers.LabelReady}, []string{providers.LabelTracking})
	assertFakeIssueLabels(t, server, 17, []string{providers.LabelReady}, []string{providers.LabelStale})
	assertFakeIssueLabels(t, server, 18, []string{providers.LabelReady}, []string{providers.LabelStale})
	assertFakeIssueLabels(t, server, 19, []string{providers.LabelStale}, nil)
	assertFakeIssueLabels(t, server, 20, []string{providers.LabelReady}, nil)
	assertFakeIssueLabels(t, server, 21, []string{providers.LabelReady}, []string{providers.LabelClaimed})
	assertFakeIssueLabels(t, server, 22, []string{providers.LabelStale}, nil)

	assertBacklogReconciliationComments(t, server, []int{7, 9, 10, 11, 15, 17, 18, 21})
	assertClaimReleaseComment(t, server, 7, "historical-run")
	claim, err := provider.ClaimWorkItem(context.Background(), providers.ClaimWorkItemRequest{
		Repository: repo,
		ID:         "7",
		RunID:      "later-run",
	})
	if err != nil {
		t.Fatalf("claim recovered issue 7: %v", err)
	}
	if !claim.Claimed || claim.ClaimedBy != "later-run" {
		t.Fatalf("reclaimed issue 7 = %+v, want later-run to win", claim)
	}
	if _, err := provider.ReleaseWorkItemClaim(context.Background(), providers.ClaimWorkItemRequest{
		Repository: repo,
		ID:         "7",
		RunID:      "later-run",
	}); err != nil {
		t.Fatalf("release follow-up claim: %v", err)
	}
	server.mu.Lock()
	beforeComments := make(map[int]int, len(server.issues))
	for id, issue := range server.issues {
		beforeComments[id] = len(issue.comments)
	}
	server.mu.Unlock()

	if reconciled, err := reconcileBacklogMetadata(context.Background(), layoutFor(root), provider, repo, "goobers:approved", defaultBacklogStalenessPolicy(), func() time.Time { return now }); err != nil {
		t.Fatalf("second reconcileBacklogMetadata: %v", err)
	} else if reconciled != 0 {
		t.Fatalf("second reconciliation count = %d, want 0", reconciled)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	for id, issue := range server.issues {
		if got := len(issue.comments); got != beforeComments[id] {
			t.Fatalf("clean second sweep added a comment to issue %d: %d -> %d", id, beforeComments[id], got)
		}
	}
}

func assertBacklogReconciliationComments(t *testing.T, server *fakeGitHubServer, ids []int) {
	t.Helper()
	for _, id := range ids {
		server.mu.Lock()
		comments := append([]string(nil), server.issues[id].comments...)
		server.mu.Unlock()
		if !strings.Contains(comments[len(comments)-1], "Goobers backlog reconciliation corrected metadata drift") {
			t.Fatalf("issue %d comments = %q, want reconciliation explanation", id, comments)
		}
	}
}

func assertClaimReleaseComment(t *testing.T, server *fakeGitHubServer, id int, runID string) {
	t.Helper()
	server.mu.Lock()
	comments := append([]string(nil), server.issues[id].comments...)
	server.mu.Unlock()
	for _, comment := range comments {
		if strings.Contains(comment, "goobers-claim-release: run="+runID) {
			return
		}
	}
	t.Fatalf("issue %d comments = %q, want release marker for %s", id, comments, runID)
}

func TestReconcileBacklogMetadataSkipsHistoricalClosedItems(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()

	server.addIssue(7, "Open drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.addIssue(8, "Recent closed drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.addIssue(9, "Historical closed drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.mu.Lock()
	server.issues[7].createdAt = now.Add(-200 * 24 * time.Hour)
	server.issues[7].updatedAt = now.Add(-200 * 24 * time.Hour)
	server.issues[8].state = "closed"
	server.issues[8].createdAt = now.Add(-200 * 24 * time.Hour)
	server.issues[8].updatedAt = now.Add(-30 * 24 * time.Hour)
	server.issues[9].state = "closed"
	server.issues[9].createdAt = now.Add(-200 * 24 * time.Hour)
	server.issues[9].updatedAt = now.Add(-120 * 24 * time.Hour)
	server.mu.Unlock()

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	provider := server.newGitHubProvider("token")
	reconciled, err := reconcileBacklogMetadata(context.Background(), layoutFor(root), provider, repo, "goobers:approved", defaultBacklogStalenessPolicy(), func() time.Time { return now })
	if err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	if reconciled != 2 {
		t.Fatalf("reconciliations = %d, want open and recent closed corrections only", reconciled)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 9, []string{providers.LabelReady, providers.LabelNeedsHuman}, nil)

	server.mu.Lock()
	queries := append([]string(nil), server.issueListQueries...)
	server.mu.Unlock()
	if len(queries) == 0 {
		t.Fatalf("issue list queries = %q, want open and recent closed listings", queries)
	}
	openQuery, err := url.ParseQuery(queries[0])
	if err != nil {
		t.Fatalf("parse open query %q: %v", queries[0], err)
	}
	if openQuery.Get("state") != "open" || openQuery.Get("since") != "" {
		t.Fatalf("open query = %q, want state=open without since", queries[0])
	}
	wantSince := now.Add(-defaultBacklogStalenessPolicy().threshold()).Format(time.RFC3339)
	sawClosed := false
	for _, raw := range queries[1:] {
		query, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatalf("parse query %q: %v", raw, err)
		}
		if query.Get("state") != "closed" {
			continue
		}
		sawClosed = true
		if query.Get("since") != wantSince {
			t.Fatalf("closed query since = %q, want %q", query.Get("since"), wantSince)
		}
	}
	if !sawClosed {
		t.Fatalf("issue list queries = %q, want recent closed listing", queries)
	}
}

func TestReconcileBacklogMetadataBoundsLargeMostlyClosedBacklog(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "10")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	server.addIssue(7, "Open drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	for id := 8; id < 158; id++ {
		server.addIssue(id, fmt.Sprintf("Closed drift %d", id), "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
		server.setIssueState(id, "closed")
		server.setIssueUpdatedAt(id, now.Add(-24*time.Hour))
	}

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	result, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadataDetailed: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want corrections bounded by the request budget", result.Reconciled)
	}
	if result.Scan.Complete || !result.Scan.WorkRemaining || result.Scan.Examined == 0 || result.Scan.Spent > result.Scan.Budget {
		t.Fatalf("scan = %#v, want bounded partial scan with work remaining", result.Scan)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelReady, providers.LabelNeedsHuman}, nil)
	assertFakeIssueLabels(t, server, 9, []string{providers.LabelReady, providers.LabelNeedsHuman}, nil)
	if got := server.issueListPageSizeHistory(); len(got) < 2 {
		t.Fatalf("list page sizes = %v, want open and closed cursor walks", got)
	}
}

func TestReconcileBacklogMetadataRequestBudgetPersistsProgress(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "10")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	for id := 7; id <= 12; id++ {
		server.addIssue(id, fmt.Sprintf("Open drift %d", id), "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}

	first, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if first.Scan.Spent > first.Scan.Budget || first.Scan.Complete || !first.Scan.WorkRemaining {
		t.Fatalf("first scan = %#v, want request-budgeted partial pass", first.Scan)
	}
	if first.Reconciled == 0 || first.Reconciled >= 6 {
		t.Fatalf("first reconciled = %d, want bounded progress but not full completion", first.Reconciled)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	server.mu.Lock()
	commentsAfterFirst := len(server.issues[7].comments)
	server.mu.Unlock()

	second, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now.Add(time.Minute) },
	)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Reconciled == 0 {
		t.Fatalf("second result = %#v, want restart progress from the persisted cursor", second)
	}
	server.mu.Lock()
	commentsAfterSecond := len(server.issues[7].comments)
	server.mu.Unlock()
	if commentsAfterSecond != commentsAfterFirst {
		t.Fatalf("issue 7 comments = %d -> %d, want persisted cursor to avoid reprocessing completed item", commentsAfterFirst, commentsAfterSecond)
	}
}

func TestReconcileBacklogMetadataDefersOverBudgetTrackingInspection(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "10")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	server.addIssue(7, "Tracking parent with many checklist children", "goobers:approved", providers.LabelTracking, providers.LabelReady)
	var body strings.Builder
	for id := 100; id < 112; id++ {
		server.addIssue(id, fmt.Sprintf("Closed child %d", id))
		server.setIssueState(id, "closed")
		fmt.Fprintf(&body, "- [x] #%d\n", id)
	}
	server.addIssue(8, "Later open drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.addIssue(9, "Later closed drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.setIssueState(9, "closed")
	server.setIssueUpdatedAt(9, now.Add(-time.Hour))
	server.mu.Lock()
	server.issues[7].body = body.String()
	server.mu.Unlock()

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	first, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if first.Reconciled != 0 || first.Scan.Complete || !first.Scan.WorkRemaining || first.Scan.OpenExamined != 1 {
		t.Fatalf("first result = %#v, want over-budget tracking item deferred without pinning completion", first)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelTracking, providers.LabelReady}, nil)

	second, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now.Add(time.Minute) },
	)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Reconciled == 0 || second.Scan.OpenExamined == 0 || second.Scan.ClosedExamined == 0 {
		t.Fatalf("second result = %#v, want later open items and the closed phase serviced after restart", second)
	}
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	servicedClosed := second.Scan.ClosedExamined > 0
	for attempt := 0; attempt < 5 && !servicedClosed; attempt++ {
		result, err := reconcileBacklogMetadataDetailed(
			context.Background(),
			layoutFor(root),
			server.newGitHubProvider("token"),
			repo,
			"goobers:approved",
			defaultBacklogStalenessPolicy(),
			func() time.Time { return now.Add(time.Duration(attempt+2) * time.Minute) },
		)
		if err != nil {
			t.Fatalf("follow-up reconcile %d: %v", attempt+1, err)
		}
		if result.Scan.Spent > result.Scan.Budget {
			t.Fatalf("follow-up result = %#v, exceeded budget", result)
		}
		servicedClosed = result.Scan.ClosedExamined > 0
	}
	if !servicedClosed {
		t.Fatal("closed phase was not serviced across restarts")
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelTracking, providers.LabelReady}, nil)
}

func TestReconcileBacklogMetadataResumesOversizedTrackingChildren(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "20")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	server.addIssue(7, "Oversized closed tracking parent", "goobers:approved", providers.LabelTracking, providers.LabelReady)
	var body strings.Builder
	for id := 100; id < 120; id++ {
		server.addIssue(id, fmt.Sprintf("Closed child %d", id))
		server.setIssueState(id, "closed")
		fmt.Fprintf(&body, "- [x] #%d\n", id)
	}
	server.mu.Lock()
	server.issues[7].body = body.String()
	server.mu.Unlock()

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	var final backlogReconciliationResult
	history := make([]string, 0, 16)
	for attempt := 0; attempt < 16; attempt++ {
		result, err := reconcileBacklogMetadataDetailed(
			context.Background(),
			layoutFor(root),
			server.newGitHubProvider("token"),
			repo,
			"goobers:approved",
			defaultBacklogStalenessPolicy(),
			func() time.Time { return now.Add(time.Duration(attempt) * time.Minute) },
		)
		if err != nil {
			t.Fatalf("reconcile attempt %d: %v", attempt+1, err)
		}
		if result.Scan.Spent > result.Scan.Budget {
			t.Fatalf("attempt %d result = %#v, exceeded budget", attempt+1, result)
		}
		final = result
		child := result.nextCursor.Open.Child
		childState := "<nil>"
		if child != nil {
			childState = fmt.Sprintf("%s/%s/%d/%s", child.ParentID, child.Fingerprint[:min(8, len(child.Fingerprint))], child.NextIndex, child.Phase)
		}
		history = append(history, fmt.Sprintf("%d: rec=%d spent=%d cursor=%q deferred=%q child=%s", attempt+1, result.Reconciled, result.Scan.Spent, result.nextCursor.Open.Cursor, result.nextCursor.Open.Deferred, childState))
		if !server.issueHasLabel(7, providers.LabelTracking) {
			break
		}
	}
	if server.issueHasLabel(7, providers.LabelTracking) {
		t.Fatalf("tracking label still present after budgeted retries; final result = %#v\nhistory:\n%s", final, strings.Join(history, "\n"))
	}
	if final.Reconciled == 0 {
		t.Fatalf("final result = %#v, want tracking correction after resumed child inspection", final)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelReady}, []string{providers.LabelTracking})
}

func TestReconcileBacklogMetadataHTTPBudgetCountsPaginatedChildrenAndUpdate(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "100")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	server.addIssue(7, "Tracking parent", "goobers:approved", providers.LabelTracking, providers.LabelReady)
	for id := 100; id < 201; id++ {
		server.addIssue(id, fmt.Sprintf("Closed child %d", id))
		server.setIssueState(id, "closed")
		server.addChild(7, id)
	}
	counter := &requestCountingHTTPClient{}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}

	result, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token", func(p *providers.GitHubProvider) { p.Client = counter }),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadataDetailed: %v", err)
	}
	if got := counter.Count(); got > result.Scan.Budget || got != result.Scan.Spent {
		t.Fatalf("actual HTTP requests=%d scan=%#v, want actual requests counted and bounded", got, result.Scan)
	}
	if result.Reconciled != 1 {
		t.Fatalf("result = %#v, want compound tracking update to complete within budget", result)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelReady}, []string{providers.LabelTracking})
}

func TestApplyBacklogMetadataCorrectionReservesCompoundMutationPastDeadline(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Deadline split correction", "goobers:approved", providers.LabelReady)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	clockReads := 0
	now := func() time.Time {
		clockReads++
		if clockReads >= 4 {
			return start.Add(defaultBacklogReconcileTimeBudget + time.Second)
		}
		return start
	}
	budget := newBacklogReconcileBudget(5, start, now)
	provider := server.newGitHubProvider("token")
	restoreClient := installBacklogReconcileBudget(provider, budget)
	defer restoreClient()

	err := applyBacklogMetadataCorrection(
		context.Background(),
		provider,
		repo,
		providers.WorkItem{ID: "7"},
		backlogMetadataCorrection{
			addLabels:    []string{providers.LabelNeedsHuman},
			removeLabels: []string{providers.LabelReady},
			reasons:      []string{"deadline split regression"},
		},
		budget,
	)
	if err != nil {
		t.Fatalf("applyBacklogMetadataCorrection: %v", err)
	}
	if budget.Spent() != 5 {
		t.Fatalf("spent = %d, want exact compound mutation request cost", budget.Spent())
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	foundComment := false
	for _, body := range fakeIssueCommentBodies(server, 7) {
		if strings.Contains(body, "deadline split regression") {
			foundComment = true
		}
	}
	if !foundComment {
		t.Fatal("correction comment was not written")
	}
}

func TestReconcileBacklogMetadataElapsedDeadlineStopsCommentPagination(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "50")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	server.addIssue(7, "Stale item with long comment history", "goobers:approved", providers.LabelReady, providers.LabelStale)
	server.mu.Lock()
	server.issues[7].createdAt = start.Add(-100 * 24 * time.Hour)
	server.mu.Unlock()
	for i := 0; i < 250; i++ {
		server.addCommentAtAs(7, "mona", fmt.Sprintf("activity %d", i), start.Add(-time.Hour))
	}
	counter := &requestCountingHTTPClient{}
	now := func() time.Time {
		if counter.Count() >= 4 {
			return start.Add(defaultBacklogReconcileTimeBudget + time.Second)
		}
		return start
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}

	result, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token", func(p *providers.GitHubProvider) { p.Client = counter }),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		now,
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadataDetailed: %v", err)
	}
	if got := counter.Count(); got > result.Scan.Budget || got >= 6 {
		t.Fatalf("actual HTTP requests=%d scan=%#v, want deadline to stop paginated comments early", got, result.Scan)
	}
	if result.Reconciled != 0 || result.Scan.Complete || !result.Scan.WorkRemaining {
		t.Fatalf("result = %#v, want incomplete pass without authoritative correction", result)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelStale}, nil)
}

func TestReconcileBacklogMetadataElapsedDeadlinePersistsProgress(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "50")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for id := 7; id <= 10; id++ {
		server.addIssue(id, fmt.Sprintf("Deadline drift %d", id), "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	calls := 0
	clock := func() time.Time {
		calls++
		if calls > 8 {
			return start.Add(defaultBacklogReconcileTimeBudget + time.Second)
		}
		return start
	}

	first, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		clock,
	)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if first.Scan.Examined == 0 || first.Scan.Complete || !first.Scan.WorkRemaining {
		t.Fatalf("first result = %#v, want elapsed-deadline partial cursor progress", first)
	}

	second, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return start.Add(time.Minute) },
	)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Reconciled == 0 {
		t.Fatalf("second result = %#v, want restart progress after elapsed cutoff", second)
	}
}

func TestReconcileBacklogMetadataEventuallyCompletesAcrossBudgetedRuns(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "100")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Now().UTC()
	for id := 7; id <= 9; id++ {
		server.addIssue(id, fmt.Sprintf("Open drift %d", id), "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	}
	for id := 10; id <= 12; id++ {
		server.addIssue(id, fmt.Sprintf("Closed drift %d", id), "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
		server.setIssueState(id, "closed")
		server.setIssueUpdatedAt(id, now.Add(-time.Hour))
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	total := 0
	var last backlogReconciliationResult
	for attempt := 0; attempt < 30; attempt++ {
		result, err := reconcileBacklogMetadataDetailed(
			context.Background(),
			layoutFor(root),
			server.newGitHubProvider("token"),
			repo,
			"goobers:approved",
			defaultBacklogStalenessPolicy(),
			func() time.Time { return now.Add(time.Duration(attempt) * time.Minute) },
		)
		if err != nil {
			t.Fatalf("reconcile attempt %d: %v", attempt+1, err)
		}
		total += result.Reconciled
		last = result
		if result.Scan.Complete {
			break
		}
	}
	if !last.Scan.Complete || total != 6 {
		t.Fatalf("total=%d last=%#v, want eventual complete correction across budgeted runs", total, last)
	}
	for id := 7; id <= 12; id++ {
		assertFakeIssueLabels(t, server, id, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	}
}

func TestReconcileBacklogMetadataCursorResumeWrapsAcrossRestarts(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "20")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	server.addIssue(7, "Open drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	for id := 8; id <= 10; id++ {
		server.addIssue(id, fmt.Sprintf("Closed drift %d", id), "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
		server.setIssueState(id, "closed")
		server.setIssueUpdatedAt(id, now.Add(-24*time.Hour))
	}

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	first, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if first.Reconciled == 0 || !first.Scan.WorkRemaining || first.Scan.OpenExamined == 0 || first.Scan.ClosedExamined == 0 {
		t.Fatalf("first result = %#v, want bounded open plus closed progress", first)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})

	second, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Reconciled == 0 || second.Scan.Spent > second.Scan.Budget {
		t.Fatalf("second result = %#v, want continued cursor progress", second)
	}

	server.addIssue(11, "New open drift after the prior cursor", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	third, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now.Add(time.Minute) },
	)
	if err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if third.Reconciled == 0 || third.Scan.OpenExamined == 0 || third.Scan.Spent > third.Scan.Budget {
		t.Fatalf("third result = %#v, want bounded open reservation progress", third)
	}
	assertFakeIssueLabels(t, server, 11, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})

	fourth, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now.Add(2 * time.Minute) },
	)
	if err != nil {
		t.Fatalf("fourth reconcile: %v", err)
	}
	if fourth.Scan.OpenExamined == 0 || fourth.Scan.Spent > fourth.Scan.Budget {
		t.Fatalf("fourth result = %#v, want open cursor to keep moving fairly", fourth)
	}
	assertFakeIssueLabels(t, server, 10, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
}

func TestReconcileBacklogMetadataCleansClosedClaimWithinWindow(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "30")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	server.addIssue(7, "Closed orphaned claim", "goobers:approved", providers.LabelClaimed)
	server.addComment(7, ownInstanceClaimBreadcrumb(t, "historical-run"))
	server.setIssueState(7, "closed")
	server.setIssueUpdatedAt(7, now.Add(-24*time.Hour))

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	result, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadataDetailed: %v", err)
	}
	if result.Reconciled != 1 || !result.Scan.Complete {
		t.Fatalf("result = %#v, want closed claim cleanup in a complete pass", result)
	}
	assertFakeIssueLabels(t, server, 7, nil, []string{providers.LabelClaimed})
}

func TestBacklogReconcileReportsPartialScanTruthfully(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "First drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.addIssue(8, "Second drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "reconcile-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "20")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "backlog-reconciliation.json")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--reconcile", root)
	if code != 0 {
		t.Fatalf("backlog-query --reconcile: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "work remains") || !strings.Contains(stderr, "work remains") {
		t.Fatalf("stdout=%q stderr=%q, want partial scan disclosure", stdout, stderr)
	}
	data, err := os.ReadFile("backlog-reconciliation.json")
	if err != nil {
		t.Fatalf("read reconciliation result: %v", err)
	}
	var result backlogReconciliationResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode reconciliation result: %v", err)
	}
	if result.Reconciled == 0 || result.Scan.Spent > result.Scan.Budget || !result.Scan.WorkRemaining || result.Scan.Complete {
		t.Fatalf("result = %#v, want bounded corrections and incomplete scan provenance", result)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
}

func TestBacklogReconcileFailsClosedWhenCursorAdvanceFails(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "20")
	resultFile := filepath.Join(t.TempDir(), "backlog-reconciliation.json")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for id := 7; id <= 10; id++ {
		server.addIssue(id, fmt.Sprintf("Drift %d", id), "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	}

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	realOpen := openStageStateStore
	failAdvance := true
	openStageStateStore = func(l instance.Layout) (stateclient.Store, error) {
		store, err := realOpen(l)
		if err != nil {
			return nil, err
		}
		return failingUpdateStore{Store: store, fail: func(key, operation string) bool {
			return failAdvance && strings.HasPrefix(key, "backlog-reconcile-") && operation == claimLockOperationBacklogScanCursor
		}}, nil
	}
	t.Cleanup(func() { openStageStateStore = realOpen })

	var stderr strings.Builder
	result, code := performBacklogQueryReconciliation(
		context.Background(),
		backlogQueryEnv{
			layout:          layoutFor(root),
			repo:            repo,
			backlogRepo:     repo,
			ghIssueProvider: server.newGitHubProvider("token"),
			stderr:          &stderr,
		},
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now().UTC(),
		resultFile,
	)
	if code == 0 {
		t.Fatalf("performBacklogQueryReconciliation code = 0, result = %#v; want fatal cursor advance failure", result)
	}
	if !strings.Contains(stderr.String(), "advance backlog reconciliation cursor") {
		t.Fatalf("stderr = %q, want cursor advance failure surfaced", stderr.String())
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read failure result: %v", err)
	}
	if strings.Contains(string(data), `"complete":true`) || strings.Contains(string(data), `"reconciled"`) {
		t.Fatalf("failure result = %s, want typed error without authoritative reconciliation summary", data)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 9, []string{providers.LabelReady, providers.LabelNeedsHuman}, nil)

	failAdvance = false
	key := backlogReconcileCursorKey(repo, providerGaggle(), "goobers:approved", defaultBacklogStalenessPolicy())
	store, err := realOpen(layoutFor(root))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	value, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("read reconcile cursor: %v", err)
	}
	if value.Exists() {
		t.Fatalf("cursor advanced despite failed write: %s", value.Data)
	}
	second, err := reconcileBacklogMetadataDetailed(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Scan.OpenExamined == 0 || !second.Scan.WorkRemaining {
		t.Fatalf("second result = %#v, want restart from unadvanced cursor with remaining work", second)
	}
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
}

func TestBacklogReconcileFailsClosedWhenClaimCursorAdvanceFails(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "20")
	resultFile := filepath.Join(t.TempDir(), "backlog-reconciliation.json")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(8, "Live claim missing marker", "goobers:approved")
	now := time.Now().UTC()
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(
		localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "8"},
		"live-run", "implementation", time.Hour,
	); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	realOpen := openStageStateStore
	updates := 0
	openStageStateStore = func(l instance.Layout) (stateclient.Store, error) {
		store, err := realOpen(l)
		if err != nil {
			return nil, err
		}
		return failingUpdateStore{Store: store, fail: func(key, operation string) bool {
			if !strings.HasPrefix(key, "backlog-reconcile-") || operation != claimLockOperationBacklogScanCursor {
				return false
			}
			updates++
			return updates == 2
		}}, nil
	}
	t.Cleanup(func() { openStageStateStore = realOpen })

	var stderr strings.Builder
	result, code := performBacklogQueryReconciliation(
		context.Background(),
		backlogQueryEnv{
			layout:          layoutFor(root),
			repo:            repo,
			backlogRepo:     repo,
			ghIssueProvider: server.newGitHubProvider("token"),
			stderr:          &stderr,
		},
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		now,
		resultFile,
	)
	if code == 0 {
		t.Fatalf("performBacklogQueryReconciliation code = 0, result = %#v; want fatal claim cursor advance failure", result)
	}
	if !strings.Contains(stderr.String(), "advance backlog reconciliation cursor") {
		t.Fatalf("stderr = %q, want cursor advance failure surfaced", stderr.String())
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read failure result: %v", err)
	}
	if strings.Contains(string(data), `"complete":true`) || strings.Contains(string(data), `"reconciled"`) {
		t.Fatalf("failure result = %s, want typed error without authoritative reconciliation summary", data)
	}
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelClaimed}, nil)

	failKey := backlogReconcileCursorKey(repo, providerGaggle(), "goobers:approved", defaultBacklogStalenessPolicy())
	store, err := realOpen(layoutFor(root))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	value, err := store.Get(context.Background(), failKey)
	if err != nil {
		t.Fatalf("read reconcile cursor: %v", err)
	}
	if !value.Exists() {
		t.Fatalf("metadata cursor was not persisted before claim cursor failure")
	}
	var cursor backlogReconcileCursorState
	if err := json.Unmarshal(value.Data, &cursor); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if cursor.Claim != "" {
		t.Fatalf("claim cursor = %q, want unadvanced cursor after failed write", cursor.Claim)
	}

	openStageStateStore = realOpen
	var retryStderr strings.Builder
	retry, retryCode := performBacklogQueryReconciliation(
		context.Background(),
		backlogQueryEnv{
			layout:          layoutFor(root),
			repo:            repo,
			backlogRepo:     repo,
			ghIssueProvider: server.newGitHubProvider("token"),
			stderr:          &retryStderr,
		},
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		now,
		"",
	)
	if retryCode != 0 || retry.Scan.WorkRemaining {
		t.Fatalf("retry code=%d result=%#v stderr=%q, want safe retry to complete", retryCode, retry, retryStderr.String())
	}
}

func TestBacklogReconcileTinyScanLimitStillRepairsClaimVisibility(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Metadata drift", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.addIssue(8, "Live claim missing marker", "goobers:approved")

	now := time.Now().UTC()
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(
		localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "8"},
		"live-run", "implementation", time.Hour,
	); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}

	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_INPUT_RECONCILESCANLIMIT", "20")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	var stderr strings.Builder
	result, code := performBacklogQueryReconciliation(
		context.Background(),
		backlogQueryEnv{
			layout:          layoutFor(root),
			repo:            repo,
			backlogRepo:     repo,
			ghIssueProvider: server.newGitHubProvider("token"),
			stderr:          &stderr,
		},
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		now,
		"",
	)
	if code != 0 {
		t.Fatalf("performBacklogQueryReconciliation: code=%d stderr=%q", code, stderr.String())
	}
	if result.Reconciled != 2 || result.Scan.Budget != 20 || result.Scan.ClaimExamined != 1 || result.Scan.Spent > result.Scan.Budget {
		t.Fatalf("result = %#v, want minimum fair budget to cover metadata and claim visibility", result)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelClaimed}, nil)
}

func TestRestoreInvisibleClaimsWindowBudgetDoesNotLeavePartialClaimEpoch(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(8, "Live claim missing marker", "goobers:approved")

	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(
		localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "8"},
		"live-run", "implementation", time.Hour,
	); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}

	var stderr strings.Builder
	first, err := restoreInvisibleClaimsWindow(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		now,
		func() time.Time { return now },
		&stderr,
		3,
		"",
	)
	if err != nil {
		t.Fatalf("first restoreInvisibleClaimsWindow: %v", err)
	}
	if first.Complete || first.Restored != 0 || first.NextCursor != "8" {
		t.Fatalf("first result = %#v, want incomplete with no restored claim", first)
	}
	assertFakeIssueLabels(t, server, 8, nil, []string{providers.LabelClaimed})
	for _, body := range fakeIssueCommentBodies(server, 8) {
		if strings.Contains(body, "goobers-claim: run=live-run") {
			t.Fatalf("budget-exhausted pass wrote partial claim breadcrumb: %q", body)
		}
	}

	stderr.Reset()
	retry, err := restoreInvisibleClaimsWindow(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		now,
		func() time.Time { return now },
		&stderr,
		20,
		first.NextCursor,
	)
	if err != nil {
		t.Fatalf("retry restoreInvisibleClaimsWindow: %v", err)
	}
	if !retry.Complete || retry.Restored != 1 || retry.NextCursor != "" || retry.Spent > 20 {
		t.Fatalf("retry result = %#v, want safe retry to restore claim marker", retry)
	}
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelClaimed}, nil)
	for _, body := range fakeIssueCommentBodies(server, 8) {
		if strings.Contains(body, "goobers-claim: run=live-run") {
			t.Fatalf("claim visibility restore wrote claim breadcrumb instead of only restoring the label: %q", body)
		}
	}
}

func TestRestoreInvisibleClaimsWindowReportsDelayedMismatchIncomplete(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(8, "Live claim with delayed mismatch", "goobers:approved")

	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "8"}
	if ok, _, err := ledger.ClaimScoped(key, "live-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}
	entry, ok := ledger.LookupScoped(key)
	if !ok {
		t.Fatal("seeded claim not found")
	}
	if ok, err := ledger.RecordClaimVerification(entry, localscheduler.ClaimVerification{
		State: "ownership-mismatch", ObservedAt: now, ProviderRunID: "other-run",
	}); err != nil || !ok {
		t.Fatalf("seed mismatch verification: ok=%v err=%v", ok, err)
	}

	var stderr strings.Builder
	result, err := restoreInvisibleClaimsWindow(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
		now,
		func() time.Time { return now },
		&stderr,
		1,
		"",
	)
	if err != nil {
		t.Fatalf("restoreInvisibleClaimsWindow: %v", err)
	}
	if result.Complete || result.NextCursor != "8" || result.Examined != 1 || result.Restored != 0 {
		t.Fatalf("result = %#v, want delayed mismatch reported incomplete at item 8", result)
	}
	assertFakeIssueLabels(t, server, 8, nil, []string{providers.LabelClaimed})
}

func TestRestoreInvisibleClaimsWindowRevalidatesLeaseBeforeRestore(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(8, "Released claim missing marker", "goobers:approved")

	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "8"}
	if ok, _, err := ledger.ClaimScoped(key, "live-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}

	released := false
	baseHandler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !released && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/8") {
			released = true
			if err := ledger.ReleaseScoped(key, "live-run"); err != nil {
				t.Errorf("release live claim during provider read: %v", err)
			}
		}
		baseHandler.ServeHTTP(w, r)
	})

	var stderr strings.Builder
	result, err := restoreInvisibleClaimsWindow(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
		now,
		func() time.Time { return now },
		&stderr,
		1,
		"",
	)
	if err != nil {
		t.Fatalf("restoreInvisibleClaimsWindow: %v", err)
	}
	if result.Complete || result.NextCursor != "8" || result.Restored != 0 {
		t.Fatalf("result = %#v, want stale released lease reported incomplete without restore", result)
	}
	assertFakeIssueLabels(t, server, 8, nil, []string{providers.LabelClaimed})
}

func TestRestoreInvisibleClaimsWindowRevalidatesLeaseExpiryBeforeRestore(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(8, "Expired claim missing marker", "goobers:approved")

	observedAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	current := observedAt
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return observedAt }),
	)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "8"}
	if ok, _, err := ledger.ClaimScoped(key, "live-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}

	baseHandler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/8") {
			current = observedAt.Add(2 * time.Hour)
		}
		baseHandler.ServeHTTP(w, r)
	})

	var stderr strings.Builder
	result, err := restoreInvisibleClaimsWindow(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
		observedAt,
		func() time.Time { return current },
		&stderr,
		1,
		"",
	)
	if err != nil {
		t.Fatalf("restoreInvisibleClaimsWindow: %v", err)
	}
	if result.Complete || result.NextCursor != "8" || result.Restored != 0 {
		t.Fatalf("result = %#v, want expired lease reported incomplete without restore", result)
	}
	assertFakeIssueLabels(t, server, 8, nil, []string{providers.LabelClaimed})
}

func TestBacklogCurationClaimRunsMetadataReconciliationBeforeSelection(t *testing.T) {
	for _, maxItems := range []string{"", "1"} {
		t.Run("maxItems="+maxItems, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(7, "Orphaned claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
			server.addComment(7, ownInstanceClaimBreadcrumb(t, "historical-run"))
			server.addIssue(8, "Contradictory state", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)

			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
			t.Setenv("GOOBERS_WORKFLOW", "widget-backlog-curation")
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
			t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", providers.LabelReady+","+providers.LabelNeedsHuman)
			t.Setenv("GOOBERS_INPUT_CURATION", "true")
			t.Setenv("GOOBERS_INPUT_MAXITEMS", maxItems)
			t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")
			t.Chdir(t.TempDir())

			code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
			if code != 0 {
				t.Fatalf("backlog-query: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !strings.Contains(stdout, "no work") {
				t.Fatalf("stdout = %q, want no work after reconciliation", stdout)
			}
			assertFakeIssueLabels(t, server, 7, []string{providers.LabelReady}, []string{providers.LabelClaimed})
			assertFakeIssueLabels(t, server, 8, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
		})
	}
}

func TestRenamedCurationClaimWithDefaultCardinalityWritesEnrichedObject(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Curation candidate", "goobers:approved")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "widget-backlog-curation")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_CURATION", "true")
	resultFile := filepath.Join(t.TempDir(), "claimed-item.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	// The claim mutation appends to the worktree-relative mutation sidecar
	// (mutationsSidecarFile), so this must not run in the package directory.
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	var item curationClaimedItem
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatalf("unmarshal single curation item: %v", err)
	}
	if item.ID != "7" || item.Staleness.ThresholdDays != 90 {
		t.Fatalf("curation item = %+v, want enriched item 7", item)
	}
}

func TestReconcileBacklogMetadataAutoClosesOptedInTrackingParent(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	server.addIssue(7, "Completed opted-in tracker", "goobers:approved", providers.LabelTracking, providers.LabelAutoClose)
	server.addIssue(8, "Closed native child")
	server.addIssue(9, "Closed checklist child")
	server.addChild(7, 8)

	server.addIssue(10, "Incomplete opted-in tracker", "goobers:approved", providers.LabelTracking, providers.LabelAutoClose)
	server.addIssue(11, "Open child")
	server.addChild(10, 11)

	server.addIssue(12, "Completed non-opted tracker", "goobers:approved", providers.LabelTracking)
	server.addIssue(13, "Closed child")
	server.addChild(12, 13)

	server.mu.Lock()
	server.issues[7].body = "- [x] #9"
	server.issues[8].state = "closed"
	server.issues[9].state = "closed"
	server.issues[13].state = "closed"
	server.mu.Unlock()

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	reconciled, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	if reconciled != 2 {
		t.Fatalf("reconciliations = %d, want 2", reconciled)
	}

	assertFakeIssueState(t, server, 7, "closed")
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelAutoClose}, []string{providers.LabelTracking})
	assertFakeIssueState(t, server, 10, "open")
	assertFakeIssueLabels(t, server, 10, []string{providers.LabelTracking, providers.LabelAutoClose}, nil)
	assertFakeIssueState(t, server, 12, "open")
	assertFakeIssueLabels(t, server, 12, nil, []string{providers.LabelTracking})
}

func TestReconcileBacklogMetadataRetriesOrphanedTrackingParentClose(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(
		7,
		"Completed claimed tracker",
		"goobers:approved",
		providers.LabelClaimed,
		providers.LabelTracking,
		providers.LabelAutoClose,
	)
	server.addComment(7, ownInstanceClaimBreadcrumb(t, "historical-run"))
	server.addIssue(8, "Closed child")
	server.addChild(7, 8)
	server.setIssueState(8, "closed")

	failClose := true
	baseHandler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failClose && r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/7") {
			http.Error(w, "close failed", http.StatusInternalServerError)
			return
		}
		baseHandler.ServeHTTP(w, r)
	})

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	provider := server.newGitHubProvider("token")
	if _, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		provider,
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	); err == nil {
		t.Fatal("reconcileBacklogMetadata error = nil, want close failure")
	}
	failClose = false
	assertFakeIssueState(t, server, 7, "open")
	assertFakeIssueLabels(
		t,
		server,
		7,
		[]string{providers.LabelClaimed, providers.LabelTracking, providers.LabelAutoClose},
		nil,
	)

	reconciled, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		provider,
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("retry reconcileBacklogMetadata: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("retry reconciliations = %d, want 1", reconciled)
	}
	assertFakeIssueState(t, server, 7, "closed")
	assertFakeIssueLabels(
		t,
		server,
		7,
		[]string{providers.LabelAutoClose},
		[]string{providers.LabelClaimed, providers.LabelTracking},
	)
}

func TestReconcileBacklogMetadataRechecksChildrenAndChecklistBeforeAutoClose(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Tracker", "goobers:approved", providers.LabelTracking, providers.LabelAutoClose)
	server.addIssue(8, "Closed native child")
	server.addIssue(9, "New open checklist child")
	server.addChild(7, 8)
	server.setIssueState(8, "closed")

	parentRefreshes := 0
	childChecks := 0
	baseHandler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7") {
			parentRefreshes++
			if parentRefreshes == 2 {
				server.mu.Lock()
				server.issues[7].body = "- [ ] #9"
				server.mu.Unlock()
			}
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7/sub_issues") {
			childChecks++
		}
		baseHandler.ServeHTTP(w, r)
	})

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	reconciled, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	if reconciled != 0 {
		t.Fatalf("reconciliations = %d, want 0", reconciled)
	}
	if parentRefreshes != 2 {
		t.Fatalf("parent refreshes = %d, want 2", parentRefreshes)
	}
	if childChecks != 2 {
		t.Fatalf("native child checks = %d, want 2", childChecks)
	}
	assertFakeIssueState(t, server, 7, "open")
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelTracking, providers.LabelAutoClose}, nil)
}

func TestReconcileBacklogMetadataRevalidatesMetadataBeforeMutation(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Transient contradiction", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)

	itemRefreshes := 0
	baseHandler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7") {
			itemRefreshes++
			if itemRefreshes == 2 {
				server.mu.Lock()
				server.issues[7].labels = []string{"goobers:approved", providers.LabelReady}
				server.mu.Unlock()
			}
		}
		baseHandler.ServeHTTP(w, r)
	})

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	reconciled, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	)
	if err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	if reconciled != 0 {
		t.Fatalf("reconciliations = %d, want 0 after fresh metadata revalidation", reconciled)
	}
	if itemRefreshes != 2 {
		t.Fatalf("item refreshes = %d, want initial inspect and pre-mutation revalidation", itemRefreshes)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelReady}, []string{providers.LabelNeedsHuman})
}

func TestBacklogReconcileWritesActualCorrectionCount(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Contradictory", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.addIssue(8, "Clean", "goobers:approved", providers.LabelReady)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "reconcile-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "backlog-reconciliation.json")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--reconcile", root)
	if code != 0 {
		t.Fatalf("backlog-query --reconcile: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	data, err := os.ReadFile("backlog-reconciliation.json")
	if err != nil {
		t.Fatalf("read reconciliation result: %v", err)
	}
	var result struct {
		Reconciled int                `json:"reconciled"`
		Integrity  apiintegrity.Grade `json:"integrity"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode reconciliation result: %v", err)
	}
	if result.Reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", result.Reconciled)
	}
	if result.Integrity != apiintegrity.Unapproved {
		t.Fatalf("integrity = %q, want %q", result.Integrity, apiintegrity.Unapproved)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelReady}, nil)
}

func TestReconcileBacklogMetadataPostsCommentBeforeRemovingLabels(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Contradictory state", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman, providers.LabelClaimed)
	server.addComment(7, ownInstanceClaimBreadcrumb(t, "historical-run"))

	baseHandler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/comments") {
			http.Error(w, "comment rejected", http.StatusUnprocessableEntity)
			return
		}
		baseHandler.ServeHTTP(w, r)
	})

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	_, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	)
	if err == nil {
		t.Fatal("reconcileBacklogMetadata error = nil, want comment failure")
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelReady, providers.LabelNeedsHuman, providers.LabelClaimed}, nil)
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entries := ledger.Snapshot(); len(entries) != 0 {
		t.Fatalf("claim ledger after provider failure = %+v, want reservation released", entries)
	}
}

func TestReconcileBacklogMetadataReservationBlocksConcurrentClaim(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Orphaned claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	server.addComment(7, ownInstanceClaimBreadcrumb(t, "historical-run"))

	type claimAttempt struct {
		ok     bool
		holder string
		err    error
	}
	lockPath := filepath.Join(root, "scheduler", claimLockFileName)
	attempted := make(chan claimAttempt, 1)
	var once sync.Once
	baseHandler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7/comments") {
			once.Do(func() {
				result := claimAttempt{}
				result.err = withClaimLock(lockPath, claimLockOperationBacklogClaim, func() error {
					ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
					if err != nil {
						return err
					}
					result.ok, result.holder, err = ledger.ClaimScoped(localscheduler.ClaimKey{
						Gaggle:     "goobers",
						Provider:   string(providers.ProviderGitHub),
						ExternalID: "7",
					}, "concurrent-run", "implementation", time.Hour)
					return err
				})
				if result.ok {
					server.addComment(7, "goobers-claim-release: run=historical-run\n\nReleased by the prior owner.")
					server.addComment(7, "goobers-claim: run=concurrent-run\n\nClaimed by the concurrent run.")
				}
				attempted <- result
			})
		}
		baseHandler.ServeHTTP(w, r)
	})

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	if _, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	); err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	result := <-attempted
	if result.err != nil {
		t.Fatalf("concurrent claim attempt: %v", result.err)
	}
	if result.ok {
		t.Fatal("concurrent claimant acquired the ledger lease during provider reconciliation")
	}
	if !strings.Contains(result.holder, "/backlog-reconcile/") {
		t.Fatalf("concurrent claim holder = %q, want reconciliation reservation", result.holder)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelReady}, []string{providers.LabelClaimed})

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entries := ledger.Snapshot(); len(entries) != 0 {
		t.Fatalf("claim ledger after reconciliation = %+v, want reservation released", entries)
	}
}

func TestReconcileBacklogMetadataReleasesClaimLockBeforeProviderIO(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Orphaned claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	server.addComment(7, ownInstanceClaimBreadcrumb(t, "historical-run"))

	lockPath := filepath.Join(root, "scheduler", claimLockFileName)
	probe := make(chan error, 1)
	var once sync.Once
	baseHandler := server.server.Config.Handler
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/comments") {
			once.Do(func() {
				held, err := platformlock.TryAcquire(lockPath)
				if err == nil {
					err = held.Release()
				}
				probe <- err
			})
		}
		baseHandler.ServeHTTP(w, r)
	})

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	if _, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	); err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	if err := <-probe; err != nil {
		if errors.Is(err, platformlock.ErrHeld) {
			t.Fatal("claim lock was held during provider I/O")
		}
		t.Fatalf("probe claim lock: %v", err)
	}
}

func TestReconcileBacklogMetadataToleratesMissingChecklistTargetWithoutAutoClose(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Tracker with stale reference", "goobers:approved", providers.LabelReady, providers.LabelTracking)
	server.addIssue(8, "Unrelated contradiction", "goobers:approved", providers.LabelReady, providers.LabelNeedsHuman)
	server.addIssue(9, "Opted-in tracker with stale reference", "goobers:approved", providers.LabelTracking, providers.LabelAutoClose)
	server.mu.Lock()
	server.issues[7].body = "- [ ] #999"
	server.issues[9].body = "- [x] #998"
	server.mu.Unlock()

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	if _, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		defaultBacklogStalenessPolicy(),
		time.Now,
	); err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelReady}, []string{providers.LabelTracking})
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelNeedsHuman}, []string{providers.LabelReady})
	assertFakeIssueLabels(t, server, 9, []string{providers.LabelAutoClose}, []string{providers.LabelTracking})
	assertFakeIssueState(t, server, 9, "open")
}

func TestReconcileBacklogMetadataUsesConfiguredStaleAfter(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Inactive stale item", "goobers:approved", providers.LabelStale)
	server.addIssue(8, "Recently active stale item", "goobers:approved", providers.LabelStale)

	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	server.mu.Lock()
	server.issues[7].createdAt = now.Add(-60 * 24 * time.Hour)
	server.issues[8].createdAt = now.Add(-60 * 24 * time.Hour)
	server.mu.Unlock()
	server.addCommentAtAs(7, "maintainer", "Older activity.", now.Add(-40*24*time.Hour))
	server.addCommentAtAs(8, "maintainer", "Recent activity.", now.Add(-20*24*time.Hour))

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	if _, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		backlogStalenessPolicy{thresholdDays: 30},
		func() time.Time { return now },
	); err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelStale}, nil)
	assertFakeIssueLabels(t, server, 8, nil, []string{providers.LabelStale})
}

func TestReconcileBacklogMetadataMatchesStructuredStalenessSignal(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Younger than raised threshold", "goobers:approved", providers.LabelStale)
	server.addIssue(8, "Activity exactly at threshold", "goobers:approved", providers.LabelStale)

	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	server.mu.Lock()
	server.issues[7].createdAt = now.Add(-40 * 24 * time.Hour)
	server.issues[8].createdAt = now.Add(-120 * 24 * time.Hour)
	server.mu.Unlock()
	server.addCommentAtAs(8, "maintainer", "Still wanted.", now.Add(-90*24*time.Hour))

	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	if _, err := reconcileBacklogMetadata(
		context.Background(),
		layoutFor(root),
		server.newGitHubProvider("token"),
		repo,
		"goobers:approved",
		backlogStalenessPolicy{thresholdDays: 90},
		func() time.Time { return now },
	); err != nil {
		t.Fatalf("reconcileBacklogMetadata: %v", err)
	}
	assertFakeIssueLabels(t, server, 7, nil, []string{providers.LabelStale})
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelStale}, nil)
}

func TestTrackingChecklistIssueIDs(t *testing.T) {
	got := trackingChecklistIssueIDs("- [ ] #12 first\n* [x] done in #13\n- ordinary ref #14\n- [ ] duplicate #12")
	if strings.Join(got, ",") != "12,13" {
		t.Fatalf("trackingChecklistIssueIDs = %v, want [12 13]", got)
	}
}

func TestTrackingChecklistMarksDoNotOverrideLiveChildState(t *testing.T) {
	repo := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}
	tests := []struct {
		name       string
		body       string
		childState string
		wantOpen   bool
	}{
		{name: "checked child is still open", body: "- [x] #8", childState: "open", wantOpen: true},
		{name: "unchecked child is already complete", body: "- [ ] #8", childState: "closed", wantOpen: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newFakeGitHubServer(t, repo.Owner, repo.Name)
			server.addIssue(7, "Tracking parent")
			server.addIssue(8, "Checklist child")
			server.setIssueState(8, tt.childState)

			open, unverified, _, err := trackingItemHasOpenChildrenBudgeted(
				context.Background(),
				server.newGitHubProvider("token"),
				repo,
				providers.WorkItem{ID: "7", Body: tt.body},
				nil,
				nil,
			)
			if err != nil {
				t.Fatalf("trackingItemHasOpenChildren: %v", err)
			}
			if unverified {
				t.Fatal("unverified = true, want a verified checklist child")
			}
			if open != tt.wantOpen {
				t.Fatalf("has open children = %t, want %t for body %q with live child state %q", open, tt.wantOpen, tt.body, tt.childState)
			}
		})
	}
}

func TestTrackingChildCursorVerifiesSkippedChecklistPrefix(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(7, "Tracking parent")
	var body strings.Builder
	for id := 100; id < 108; id++ {
		server.addIssue(id, fmt.Sprintf("Closed child %d", id))
		server.setIssueState(id, "closed")
		fmt.Fprintf(&body, "- [x] #%d\n", id)
	}
	provider := server.newGitHubProvider("token")
	budget := newBacklogReconcileBudget(5, time.Now().UTC(), time.Now)
	restoreClient := installBacklogReconcileBudget(provider, budget)
	_, _, _, err := trackingItemHasOpenChildrenBudgeted(context.Background(), provider, repo, providers.WorkItem{ID: "7", Body: body.String()}, budget, nil)
	restoreClient()
	var childErr backlogChildInspectionBudgetError
	if !errors.As(err, &childErr) || childErr.cursor.NextIndex == 0 {
		t.Fatalf("trackingItemHasOpenChildrenBudgeted err = %v, want child cursor budget error", err)
	}

	server.setIssueState(100, "open")
	provider = server.newGitHubProvider("token")
	budget = newBacklogReconcileBudget(50, time.Now().UTC(), time.Now)
	restoreClient = installBacklogReconcileBudget(provider, budget)
	open, _, _, err := trackingItemHasOpenChildrenBudgeted(context.Background(), provider, repo, providers.WorkItem{ID: "7", Body: body.String()}, budget, &childErr.cursor)
	restoreClient()
	if err == nil || !errors.As(err, &childErr) || childErr.cursor.Phase != backlogChildInspectionPhaseVerify {
		t.Fatalf("resume err = %v, cursor = %#v, want verification cursor before authoritative completion", err, childErr.cursor)
	}
	if open {
		t.Fatal("resume open = true before verification phase, want deferred verification")
	}

	provider = server.newGitHubProvider("token")
	budget = newBacklogReconcileBudget(50, time.Now().UTC(), time.Now)
	restoreClient = installBacklogReconcileBudget(provider, budget)
	open, _, _, err = trackingItemHasOpenChildrenBudgeted(context.Background(), provider, repo, providers.WorkItem{ID: "7", Body: body.String()}, budget, &childErr.cursor)
	restoreClient()
	if err != nil {
		t.Fatalf("verify tracking children: %v", err)
	}
	if !open {
		t.Fatal("verify open = false, want reopened skipped child detected")
	}
}

// TestParseBacklogReconcileRunIDRoundTripsFormat pins the shape
// reserveBacklogClaimReconciliation persists into the claim ledger:
// claims.go's claimHolderTerminal must keep parsing it to recover the
// owning run, so the format/parse pair must stay inverses, and anything
// that only superficially resembles the shape (wrong segment count, a
// non-numeric pid/seq, or an empty owner) must not parse.
func TestParseBacklogReconcileRunIDRoundTripsFormat(t *testing.T) {
	got := formatBacklogReconcileRunID("run-123", 4242, 7)
	want := "run-123/backlog-reconcile/4242/7"
	if got != want {
		t.Fatalf("formatBacklogReconcileRunID = %q, want %q", got, want)
	}
	owner, ok := parseBacklogReconcileRunID(got)
	if !ok || owner != "run-123" {
		t.Fatalf("parseBacklogReconcileRunID(%q) = (%q, %v), want (\"run-123\", true)", got, owner, ok)
	}

	for _, malformed := range []string{
		"",
		"run-123",
		"run-123/backlog-reconcile/4242",
		"run-123/backlog-reconcile/not-a-pid/7",
		"run-123/backlog-reconcile/4242/not-a-seq",
		"/backlog-reconcile/4242/7",
		"run-123/backlog-reconcile/4242/7/extra",
		"run-123/some-other-kind/4242/7",
	} {
		if owner, ok := parseBacklogReconcileRunID(malformed); ok {
			t.Fatalf("parseBacklogReconcileRunID(%q) = (%q, true), want ok=false", malformed, owner)
		}
	}
}

func defaultBacklogStalenessPolicy() backlogStalenessPolicy {
	return backlogStalenessPolicy{thresholdDays: int(defaultStaleAfter / (24 * time.Hour))}
}

func reconcileBacklogMetadata(
	ctx context.Context,
	l instance.Layout,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	now func() time.Time,
) (int, error) {
	result, err := reconcileBacklogMetadataDetailed(ctx, l, provider, repo, trustLabel, stalenessPolicy, now)
	return result.Reconciled, err
}

type failingUpdateStore struct {
	stateclient.Store
	fail func(key, operation string) bool
}

func (s failingUpdateStore) Update(ctx context.Context, key, operation string, fn func(stateclient.Value) ([]byte, bool, error)) error {
	if s.fail != nil && s.fail(key, operation) {
		return errors.New("injected scheduler-state update failure")
	}
	return s.Store.Update(ctx, key, operation, fn)
}

type requestCountingHTTPClient struct {
	mu    sync.Mutex
	count int
	inner providers.HTTPClient
}

func (c *requestCountingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	if c.inner != nil {
		return c.inner.Do(req)
	}
	return http.DefaultClient.Do(req)
}

func (c *requestCountingHTTPClient) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func fakeIssueCommentBodies(server *fakeGitHubServer, id int) []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	issue := server.issues[id]
	if issue == nil {
		return nil
	}
	return append([]string(nil), issue.comments...)
}

func assertFakeIssueLabels(t *testing.T, server *fakeGitHubServer, id int, want, reject []string) {
	t.Helper()
	server.mu.Lock()
	labels := append([]string(nil), server.issues[id].labels...)
	server.mu.Unlock()
	for _, label := range want {
		if !hasAllLabels(labels, []string{label}) {
			t.Fatalf("issue %d labels = %v, want %q", id, labels, label)
		}
	}
	for _, label := range reject {
		if hasAllLabels(labels, []string{label}) {
			t.Fatalf("issue %d labels = %v, reject %q", id, labels, label)
		}
	}
}

func assertFakeIssueState(t *testing.T, server *fakeGitHubServer, id int, want string) {
	t.Helper()
	server.mu.Lock()
	got := server.issues[id].state
	server.mu.Unlock()
	if got != want {
		t.Fatalf("issue %d state = %q, want %q", id, got, want)
	}
}
