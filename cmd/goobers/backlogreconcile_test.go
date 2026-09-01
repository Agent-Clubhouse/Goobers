package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/providers"
)

func TestReconcileBacklogMetadataRepairsDriftAndLeavesCorrectLabelsUntouched(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
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

	for _, id := range []int{7, 9, 10, 11, 15, 17, 18, 21} {
		server.mu.Lock()
		comments := append([]string(nil), server.issues[id].comments...)
		server.mu.Unlock()
		if !strings.Contains(comments[len(comments)-1], "Goobers backlog reconciliation corrected metadata drift") {
			t.Fatalf("issue %d comments = %q, want reconciliation explanation", id, comments)
		}
	}
	server.mu.Lock()
	comments := append([]string(nil), server.issues[7].comments...)
	server.mu.Unlock()
	var releasedOrphan bool
	for _, comment := range comments {
		if strings.Contains(comment, "goobers-claim-release: run=historical-run") {
			releasedOrphan = true
			break
		}
	}
	if !releasedOrphan {
		t.Fatalf("issue 7 comments = %q, want release marker for historical-run", comments)
	}
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

func TestBacklogCurationClaimRunsMetadataReconciliationBeforeSelection(t *testing.T) {
	for _, maxItems := range []string{"", "1"} {
		t.Run("maxItems="+maxItems, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(7, "Orphaned claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
			server.addComment(7, "goobers-claim: run=historical-run\n\nClaimed by an earlier run.")
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
	server.addComment(7, "goobers-claim: run=historical-run\n\nClaimed by an earlier run.")
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
	server.addComment(7, "goobers-claim: run=historical-run\n\nClaimed by an earlier run.")

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
	server.addComment(7, "goobers-claim: run=historical-run\n\nClaimed by an earlier run.")

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
	server.addComment(7, "goobers-claim: run=historical-run\n\nClaimed by an earlier run.")

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
