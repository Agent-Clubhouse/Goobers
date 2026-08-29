package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func prClaimTestRepo() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
}

// TestPullRequestClaimIsScopedToRepositoryProvider is the regression for
// #3649: a scoped PR claim used to be recorded under a hardcoded github
// namespace regardless of where the pull request lived, so an ADO repository's
// PR #77 claim occupied the GitHub namespace and suppressed a GitHub run's
// claim on its own unrelated PR #77.
func TestPullRequestClaimIsScopedToRepositoryProvider(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	adoRepo := providers.RepositoryRef{
		Provider: providers.ProviderADO, Owner: "contoso", Project: "platform", Name: "web",
	}
	candidates := []providers.PullRequestSummary{{Number: 77}}

	claimed, err := claimPullRequestInOrder(root, adoRepo, candidates, "ado-run", "pr-remediation", time.Hour)
	if err != nil {
		t.Fatalf("claim ADO PR: %v", err)
	}
	if claimed == nil {
		t.Fatal("ADO PR #77 was not claimed")
	}

	claimed, err = claimPullRequestInOrder(root, prClaimTestRepo(), candidates, "github-run", "merge-review", time.Hour)
	if err != nil {
		t.Fatalf("claim GitHub PR: %v", err)
	}
	if claimed == nil {
		t.Fatal("GitHub PR #77 was suppressed by the ADO claim on a different provider's PR")
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		provider providers.ProviderKind
		runID    string
	}{
		{providers.ProviderADO, "ado-run"},
		{providers.ProviderGitHub, "github-run"},
	} {
		entry, held := ledger.LookupScoped(localscheduler.ClaimKey{
			Gaggle:     "goobers",
			Provider:   string(tc.provider),
			ExternalID: pullRequestClaimKey(77),
		})
		if !held || entry.RunID != tc.runID {
			t.Fatalf("%s claim = (%+v, %v), want run %q", tc.provider, entry, held, tc.runID)
		}
	}

	now := time.Now()
	if claimedPR, _ := pullRequestClaimStatus(ledger, "goobers", providers.ProviderADO, 77, "ado-run", now); !claimedPR {
		t.Fatal("ADO claim on PR #77 is invisible to the claim reader that must honor it")
	}
	if _, owned := pullRequestClaimStatus(ledger, "goobers", providers.ProviderGitHub, 77, "ado-run", now); owned {
		t.Fatal("GitHub PR #77 claim is attributed to the ADO run")
	}
}

// TestPullRequestClaimRejectsMissingProviderIdentity keeps the failure at the
// earliest reliable boundary: without a provider a scoped claim cannot be
// namespaced, so the claim is refused with an actionable diagnostic rather
// than silently landing in some other provider's namespace (#3649).
func TestPullRequestClaimRejectsMissingProviderIdentity(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")

	_, err := claimPullRequestInOrder(
		root,
		providers.RepositoryRef{Owner: "your-org", Name: "your-repo"},
		[]providers.PullRequestSummary{{Number: 77}},
		"run-no-provider", "pr-remediation", time.Hour,
	)
	if err == nil {
		t.Fatal("claim succeeded without a repository provider identity")
	}
	if !strings.Contains(err.Error(), "provider identity") {
		t.Fatalf("error = %q, want it to name the missing provider identity", err)
	}
}

// TestPostMergeReconcileRecordsAreProviderComplete is the regression for the
// reconciliation half of #3649: records keyed by owner/name alone let two
// distinct repositories that share an owner/name shape satisfy each other's
// reconciliation.
func TestPostMergeReconcileRecordsAreProviderComplete(t *testing.T) {
	root := initDemo(t)
	platform := providers.RepositoryRef{
		Provider: providers.ProviderADO, Owner: "contoso", Project: "platform", Name: "web",
	}
	tooling := providers.RepositoryRef{
		Provider: providers.ProviderADO, Owner: "contoso", Project: "tooling", Name: "web",
	}
	gitea := providers.RepositoryRef{
		Provider: providers.ProviderGitea, Owner: "contoso", Name: "web", URL: "https://gitea.example.com",
	}
	at := time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC)
	for _, repo := range []providers.RepositoryRef{platform, tooling, gitea} {
		if err := recordPostMergeTimeout(root, repo, "20", at); err != nil {
			t.Fatalf("record timeout for %s: %v", repo.CanonicalKey(), err)
		}
	}

	ledgerPath := filepath.Join(layoutFor(root).SchedulerDir(), postMergeReconcileLedgerFile)
	ledger, err := readPostMergeReconcileLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Entries) != 3 {
		t.Fatalf("ledger entries = %d, want one per distinct provider repository", len(ledger.Entries))
	}
	if !completePostMergeReconciliation(&ledger, platform, "20") {
		t.Fatal("completing the platform record reported no change")
	}
	if postMergeReconciliationCompleted(ledger, tooling, "20") {
		t.Fatal("the platform project's completion satisfied the tooling project's reconciliation")
	}
	if postMergeReconciliationCompleted(ledger, gitea, "20") {
		t.Fatal("an ADO completion satisfied a Gitea repository's reconciliation")
	}
	if pending := pendingPostMergeReconcileKeys(ledger, tooling); len(pending) != 1 {
		t.Fatalf("pending keys for the tooling project = %v, want exactly its own", pending)
	}
}

// TestPostMergeReconcileLedgerMigratesLegacyKeys keeps existing successful
// behavior unchanged across the key change (#3649): a record written under the
// old owner/name key still resolves, so a completed reconciliation is not
// replayed and a pending one is not stranded under an unreachable key.
func TestPostMergeReconcileLedgerMigratesLegacyKeys(t *testing.T) {
	root := initDemo(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	schedulerDir := layoutFor(root).SchedulerDir()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := postMergeReconcileLedger{
		Version: postMergeReconcileVersion,
		Entries: map[string]postMergeReconcileEntry{
			"your-org/your-repo#20": {
				Repository: repo,
				PullNumber: "20",
				State:      postMergeReconcileCompleted,
				TimedOutAt: time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC),
			},
			"your-org/your-repo#21": {
				Repository: repo,
				PullNumber: "21",
				State:      postMergeReconcilePending,
				TimedOutAt: time.Date(2026, 8, 24, 5, 0, 0, 0, time.UTC),
			},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(schedulerDir, postMergeReconcileLedgerFile)
	if err := os.WriteFile(ledgerPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	ledger, err := readPostMergeReconcileLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !postMergeReconciliationCompleted(ledger, repo, "20") {
		t.Fatal("a legacy completed record was lost, so its reconciliation would replay")
	}
	pending := pendingPostMergeReconcileKeys(ledger, repo)
	if len(pending) != 1 || pending[0] != postMergeReconcileKey(repo, "21") {
		t.Fatalf("pending keys = %v, want the legacy pending record rekeyed", pending)
	}
}
