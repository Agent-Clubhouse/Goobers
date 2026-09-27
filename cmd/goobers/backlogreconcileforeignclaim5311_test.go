package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// testOwnInstanceID is the instance identity ownInstanceClaimBreadcrumb pins
// for the stage under test.
const testOwnInstanceID = "0123456789abcdef0123456789abcdef"

// ownInstanceClaimBreadcrumb renders a historical claim breadcrumb attributed
// to the stage's own instance, the provider epoch backlog reconciliation may
// end once its ledger no longer backs it (#5311). It pins the stage's
// instance identity to match.
func ownInstanceClaimBreadcrumb(t *testing.T, runID string) string {
	t.Helper()
	t.Setenv(executor.InstanceIDEnvVar, testOwnInstanceID)
	data, err := json.Marshal(providers.Attribution{
		Schema: 1, Goobers: true, InstanceID: testOwnInstanceID, Gaggle: "goobers",
		Workflow: "implementation", Task: "claim", Goober: "deterministic", Run: runID, Action: "claim",
	})
	if err != nil {
		t.Fatal(err)
	}
	return "goobers-claim: run=" + runID + "\n\nClaimed by an earlier run.\n\n" +
		providers.AttributionMarkerPrefix + base64.StdEncoding.EncodeToString(data) + " -->"
}

// #5311: two instances share one repository (and, here, one provider login).
// Instance B's ledger knows nothing of instance A's live claim, and absence
// from B's ledger is not evidence that A's claim is stale: B must neither
// strip A's goobers:claimed label nor post a release that ends A's epoch.
// Only A, once its own ledger no longer backs the claim, may clear it.
func TestReconcileBacklogMetadataLeavesForeignInstanceClaimEpochs(t *testing.T) {
	const (
		instanceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		instanceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	rootA := initDemo(t)
	rootB := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "A's live claim", "goobers:approved", providers.LabelReady)
	server.addIssue(8, "Other identity's claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	server.addCommentAs(8, "someone-else", "goobers-claim: run=other-login-run\n\nClaimed by another identity.")
	server.addIssue(9, "Unattributed legacy claim", "goobers:approved", providers.LabelReady, providers.LabelClaimed)
	server.addComment(9, "goobers-claim: run=legacy-run\n\nClaimed by a run no instance here admitted.")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	now := time.Now()

	providerA := server.newGitHubProvider("token")
	providerA.SetAttribution(providers.Attribution{
		InstanceID: instanceA, Gaggle: "goobers", Workflow: "implementation",
		Task: "claim", Goober: "deterministic", Run: "a-live-run",
	})
	claim, err := providerA.ClaimWorkItem(context.Background(), providers.ClaimWorkItemRequest{
		Repository: repo, ID: "7", RunID: "a-live-run",
	})
	if err != nil || !claim.Claimed {
		t.Fatalf("instance A claim: claimed=%v err=%v", claim.Claimed, err)
	}
	ledgerA, err := localscheduler.OpenClaimLedger(filepath.Join(rootA, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	keyA := localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "7"}
	if ok, _, err := ledgerA.ClaimScoped(keyA, "a-live-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed instance A lease: ok=%v err=%v", ok, err)
	}

	reconcileAs := func(root, instanceID string) {
		t.Helper()
		t.Setenv(executor.InstanceIDEnvVar, instanceID)
		if _, err := reconcileBacklogMetadata(context.Background(), layoutFor(root), server.newGitHubProvider("token"),
			repo, "goobers:approved", defaultBacklogStalenessPolicy(), func() time.Time { return now }); err != nil {
			t.Fatalf("reconcileBacklogMetadata as %s: %v", instanceID, err)
		}
	}
	releasedA := func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		for _, comment := range server.issues[7].comments {
			if strings.Contains(comment, "goobers-claim-release: run=a-live-run") {
				return true
			}
		}
		return false
	}

	reconcileAs(rootB, instanceB)
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelClaimed}, nil)
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelClaimed}, nil)
	assertFakeIssueLabels(t, server, 9, []string{providers.LabelClaimed}, nil)
	if releasedA() {
		t.Fatal("instance B ended instance A's live provider claim epoch")
	}
	ledgerB, err := localscheduler.OpenClaimLedger(filepath.Join(rootB, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entries := ledgerB.Snapshot(); len(entries) != 0 {
		t.Fatalf("instance B ledger after leaving foreign claims = %+v, want its reservations released", entries)
	}
	if winner, err := providerA.ClaimWorkItem(context.Background(), providers.ClaimWorkItemRequest{
		Repository: repo, ID: "7", RunID: "b-run",
	}); err != nil || winner.Claimed || winner.ClaimedBy != "a-live-run" {
		t.Fatalf("claim by another run after B's reconciliation = %+v, %v; want a-live-run still elected", winner, err)
	}

	// A's run ends without releasing its provider marker: A's own ledger no
	// longer backs it, so A's reconciliation clears the label and ends the
	// epoch. Neither instance touches the other identity's claim.
	if err := ledgerA.ReleaseScoped(keyA, "a-live-run"); err != nil {
		t.Fatal(err)
	}
	reconcileAs(rootA, instanceA)
	assertFakeIssueLabels(t, server, 7, []string{providers.LabelReady}, []string{providers.LabelClaimed})
	if !releasedA() {
		t.Fatal("instance A did not end its own orphaned provider claim epoch")
	}
	assertFakeIssueLabels(t, server, 8, []string{providers.LabelClaimed}, nil)
	assertFakeIssueLabels(t, server, 9, []string{providers.LabelClaimed}, nil)
}
