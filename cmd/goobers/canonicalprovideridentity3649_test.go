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
	claimsLedger, err := fileClaimLedger(layoutFor(root))
	if err != nil {
		t.Fatal(err)
	}
	adoClaims, err := pullRequestClaimListing(claimsLedger, "goobers", providers.ProviderADO)
	if err != nil {
		t.Fatal(err)
	}
	if claimedPR, _ := pullRequestClaimStatus(adoClaims, "goobers", providers.ProviderADO, 77, "ado-run", now); !claimedPR {
		t.Fatal("ADO claim on PR #77 is invisible to the claim reader that must honor it")
	}
	githubClaims, err := pullRequestClaimListing(claimsLedger, "goobers", providers.ProviderGitHub)
	if err != nil {
		t.Fatal(err)
	}
	if _, owned := pullRequestClaimStatus(githubClaims, "goobers", providers.ProviderGitHub, 77, "ado-run", now); owned {
		t.Fatal("GitHub PR #77 claim is attributed to the ADO run")
	}
}

// TestPullRequestClaimStatusRecognizesLegacyGitHubScopedClaimForNonGitHubProvider
// is the merge-review regression for #3649's rollout: every build before
// that change wrote EVERY gaggle-scoped PR claim under a hardcoded
// ProviderGitHub key, regardless of the repository's real provider. A claim
// an ADO or Gitea run leased under that legacy key must stay visible to the
// new provider-scoped reader until it naturally expires — otherwise the
// first post-upgrade selection cannot see it and claims the same PR again,
// letting two runs process it concurrently.
func TestPullRequestClaimStatusRecognizesLegacyGitHubScopedClaimForNonGitHubProvider(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	schedulerDir := layoutFor(root).SchedulerDir()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed the claim the way a pre-#3649 build wrote it: hardcoded github
	// namespace even though PR #77 here belongs to an ADO repository.
	rawLedger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := rawLedger.ClaimScoped(localscheduler.ClaimKey{
		Gaggle:     "goobers",
		Provider:   string(providers.ProviderGitHub),
		ExternalID: pullRequestClaimKey(77),
	}, "legacy-run", "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed legacy github-scoped claim: ok=%t err=%v", ok, err)
	}

	ledger, err := fileClaimLedger(layoutFor(root))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	claims, err := pullRequestClaimListing(ledger, "goobers", providers.ProviderADO)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ownedByCurrentRun := pullRequestClaimStatus(claims, "goobers", providers.ProviderADO, 77, "new-run", now)
	if !claimed {
		t.Fatal("legacy github-scoped claim on an ADO PR is invisible to the provider-scoped reader")
	}
	if ownedByCurrentRun {
		t.Fatal("legacy claim held by a different run was attributed to the new run")
	}

	available, err := filterClaimAvailablePullRequests(
		ledger, "goobers", providers.ProviderADO, "new-run",
		[]providers.PullRequestSummary{{Number: 77}}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 0 {
		t.Fatalf("available = %+v, want PR #77 excluded — it is still held by the legacy claim", available)
	}

	// The run that actually holds the legacy claim must still recognize it
	// as its own: currentRunHasLivePullRequestClaim reads ForRunAll's
	// unfiltered result, so it needs the same legacy-namespace fallback.
	held, err := ledger.ForRunAll(claimContext(), "legacy-run")
	if err != nil {
		t.Fatal(err)
	}
	if !currentRunHasLivePullRequestClaim(held, "goobers", providers.ProviderADO, "legacy-run", now) {
		t.Fatal("legacy-run's own legacy github-scoped claim on an ADO PR was not recognized as its own live claim")
	}

	// Once the legacy claim expires, the same PR must become claimable again
	// under the new provider-scoped key — the fallback must not pin it
	// forever.
	expired := now.Add(2 * time.Hour)
	expiredClaims, err := pullRequestClaimListing(ledger, "goobers", providers.ProviderADO)
	if err != nil {
		t.Fatal(err)
	}
	if claimedAfterExpiry, _ := pullRequestClaimStatus(expiredClaims, "goobers", providers.ProviderADO, 77, "new-run", expired); claimedAfterExpiry {
		t.Fatal("expired legacy claim is still reported as live")
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
