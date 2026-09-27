package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// prClaimADOFixture builds an ADO instance root routed at a fake ADO server
// (the adoRemediationServerState fixture pushremediated_ado_test.go already
// wires for the pr-remediation lane) with this run's PR claim seeded, so
// pr-claim's terminal-state guard (#5655, ADO-N14) can be exercised the same
// way the GitHub arm's TestPRRemediationLifecycle* tests are.
func prClaimADOFixture(t *testing.T, status, headSHA string) (root string, repo providers.RepositoryRef) {
	t.Helper()
	root, repo = providerDispatchFixture(t, providers.ProviderADO)
	st := &adoRemediationServerState{
		owner: repo.Owner, project: repo.Project, name: repo.Name,
		prNumber: 77, prBranch: "goobers/impl/ado-claim", base: "main",
		headSHA: headSHA, baseSHA: "base-sha", status: status,
	}
	server := st.start(t)
	installADOStageProvider(t, repo, server)

	t.Setenv("GOOBERS_RUN_ID", "run-364-ado")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	// No GitHub-capability credential: ADO resolves its own credential from
	// repos[].auth, so pr-claim must reach the poll without one (an ADO repo
	// on azure-cli / workload- / managed-identity auth never receives it).
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), prRemediationLifecycleResultFile))
	if _, err := claimPullRequestInOrder(root, repo, []providers.PullRequestSummary{{Number: 77}}, "run-364-ado", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	return root, repo
}

// TestPRRemediationLifecycleADOKeepsActivePRClaim covers the ADO
// active -> Open case: an ADO PR with status "active" and a live source head
// reads as open through the narrow GetPullRequest surface (remediationStageSurface),
// which now routes ADO instead of refusing it (remediationStageProvider's
// GitHub/Gitea-only default-error arm).
func TestPRRemediationLifecycleADOKeepsActivePRClaim(t *testing.T) {
	root, _ := prClaimADOFixture(t, "active", "head-sha-live")

	code, stdout, stderr := runArgs(t, "pr-claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if stdout == "" {
		t.Fatal("stdout is empty")
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	entry, held := ledger.Lookup(pullRequestClaimKey(77))
	if !held || entry.RunID != "run-364-ado" {
		t.Fatalf("open PR claim = %+v, held=%v; want run-364-ado claim retained", entry, held)
	}
}

// TestPRRemediationLifecycleADOReleasesCompletedPRClaim covers the ADO
// completed -> released case: PollPullRequest maps ADO status "completed" to
// state "merged", so pr-claim's existing open/merged check releases the claim
// and reports terminal no-work, exactly as the GitHub arm does today.
func TestPRRemediationLifecycleADOReleasesCompletedPRClaim(t *testing.T) {
	root, _ := prClaimADOFixture(t, "completed", "head-sha-final")

	code, stdout, stderr := runArgs(t, "pr-claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no longer open") {
		t.Fatalf("stdout = %q, want a mention of the claim no longer being open", stdout)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, held := ledger.Lookup(pullRequestClaimKey(77)); held {
		t.Fatal("completed ADO PR claim remains held")
	}
}

// TestPRRemediationLifecycleADOReleasesAbandonedPRClaim covers the ADO
// abandoned -> released case: PollPullRequest maps status "abandoned" to
// state "closed", which is neither open nor merged, so the claim is released.
func TestPRRemediationLifecycleADOReleasesAbandonedPRClaim(t *testing.T) {
	root, _ := prClaimADOFixture(t, "abandoned", "head-sha-abandoned")

	code, stdout, stderr := runArgs(t, "pr-claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no longer open") {
		t.Fatalf("stdout = %q, want a mention of the claim no longer being open", stdout)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, held := ledger.Lookup(pullRequestClaimKey(77)); held {
		t.Fatal("abandoned ADO PR claim remains held")
	}
}

// TestPRRemediationLifecycleADOFailsOnEmptySourceHead is the ADO head guard:
// an ADO PR that reads "active" (open) but whose poll returns no source head
// SHA must never read as open (#5655's "verify... source head" — the head is
// observed through the same ADO poll GetPullRequest adapts, and an empty
// observation fails closed rather than letting the lane proceed against an
// unverified source).
func TestPRRemediationLifecycleADOFailsOnEmptySourceHead(t *testing.T) {
	root, _ := prClaimADOFixture(t, "active", "")

	code, _, stderr := runArgs(t, "pr-claim", root)
	if code == 0 {
		t.Fatalf("code = %d, want a non-zero exit for an empty ADO source head", code)
	}
	if !strings.Contains(stderr, "no source head SHA") {
		t.Fatalf("stderr = %q, want a mention of the missing source head SHA", stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	entry, held := ledger.Lookup(pullRequestClaimKey(77))
	if !held || entry.RunID != "run-364-ado" {
		t.Fatalf("claim = %+v, held=%v; a failed guard must not silently release the claim", entry, held)
	}
}
