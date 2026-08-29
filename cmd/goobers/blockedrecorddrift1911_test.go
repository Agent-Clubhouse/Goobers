package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// TestBacklogQueryDependencyRecheckHonorsRecordedBlockers is #1911's core AC:
// the dependency recheck resolves an item's blockers from blocked.json, so an
// item whose natively named blockers all closed stays parked while the
// recorded blocker is still open — a curator never gets the chance to clear
// the label against the record the selector still enforces.
func TestBacklogQueryDependencyRecheckHonorsRecordedBlockers(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "7"): {
			Repository: repo,
			ItemID:     "7",
			Blockers:   []string{"9"},
			RunID:      "earlier-run",
		},
	}); err != nil {
		t.Fatal(err)
	}

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Blocked item", "goobers:approved", blockedOnSiblingLabel)
	server.addIssue(8, "Cited blocker", "goobers:approved")
	server.setIssueState(8, "closed")
	server.addIssue(9, "Recorded blocker", "goobers:approved")
	server.setIssueBlockers(7, 8)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "recorded-blocker-run")
	configureCurationResweep(t, "2", "1", "24h")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed := ledger.Lookup("7"); claimed {
		t.Fatalf("item 7 was claimed for dependency recheck despite open recorded blocker 9; stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "still recorded blocked on 9") {
		t.Fatalf("stderr = %q, want a warning naming the still-open recorded blocker", stderr)
	}
}

// TestBacklogQueryDependencyRecheckProceedsWhenRecordedBlockersClosed pins the
// other half: the recorded blockers gate the recheck, they do not disable it.
func TestBacklogQueryDependencyRecheckProceedsWhenRecordedBlockersClosed(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "7"): {
			Repository: repo,
			ItemID:     "7",
			Blockers:   []string{"9"},
			RunID:      "earlier-run",
		},
	}); err != nil {
		t.Fatal(err)
	}

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Blocked item", "goobers:approved", blockedOnSiblingLabel)
	server.addIssue(8, "Native blocker", "goobers:approved")
	server.setIssueState(8, "closed")
	server.addIssue(9, "Recorded blocker", "goobers:approved")
	server.setIssueState(9, "closed")
	server.setIssueBlockers(7, 8)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "resolved-blocker-run")
	configureCurationResweep(t, "2", "1", "24h")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	items := readCurationItems(t, filepath.Join(workDir, "claimed-items.json"))
	if len(items) != 1 || items[0].ID != "7" || items[0].CurationMode != "dependency-recheck" {
		t.Fatalf("curation items = %+v, want item 7 in dependency-recheck mode", items)
	}
}

// TestFilterBlockedEligibilityReportsLabelDrift is #1911's consistency check:
// an item the record still parks, but which no longer carries the
// operator-facing label, is reported instead of silently never moving.
func TestFilterBlockedEligibilityReportsLabelDrift(t *testing.T) {
	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(458, "recorded blocker", "goobers:approved") // stays open
	server.addIssue(459, "drifted item", "goobers:approved", providers.LabelReady)

	recs := func() map[string]blockedRecord {
		return map[string]blockedRecord{
			blockedRecordKey(repo, "459"): {
				Repository: repo,
				ItemID:     "459",
				Blockers:   []string{"458"},
				RunID:      "run-1",
			},
		}
	}
	drifted := []providers.WorkItem{{ID: "459", Labels: []string{"goobers:approved", providers.LabelReady}}}

	filtered, _, _, warnings := filterBlockedEligibility(context.Background(), provider, repo, drifted, recs())
	if len(filtered) != 0 {
		t.Fatalf("filtered = %v, want 459 still excluded by its live record", filtered)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "blocked-record drift: item 459 is recorded blocked on 458") {
		t.Fatalf("warnings = %v, want a drift report naming item 459 and blocker 458", warnings)
	}

	parked := []providers.WorkItem{{ID: "459", Labels: []string{"goobers:approved", blockedOnSiblingLabel}}}
	if _, _, _, warnings = filterBlockedEligibility(context.Background(), provider, repo, parked, recs()); len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none while the label agrees with the record", warnings)
	}
}
