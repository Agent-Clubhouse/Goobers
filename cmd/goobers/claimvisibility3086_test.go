package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// seedLiveClaim gives the instance at root a live ledger lease on itemID.
func seedLiveClaim(t *testing.T, root, itemID, runID string, now time.Time) {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{
		Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: itemID,
	}
	if ok, _, err := ledger.ClaimScoped(key, runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed live claim on %s: ok=%v err=%v", itemID, ok, err)
	}
}

// TestRestoreInvisibleClaimsRepublishesTheMarker is #3086's reported
// direction.
//
// In a dogfood instance, active implementation runs held authoritative ledger
// claims for five issues while none carried goobers:claimed — the labels had
// been removed during escalation and manual recovery, and nothing put them
// back. Anyone reading GitHub saw unclaimed work that Goobers was actively
// holding, so humans and other automation made decisions on false state.
//
// The existing reconciliation handles only the opposite drift, and cannot see
// this one: it selects items to inspect BY LABEL, so an item whose labels were
// stripped is never looked at.
func TestRestoreInvisibleClaimsRepublishesTheMarker(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(41, "held but invisible")

	// The reconcile stage runs with its gaggle in the environment; the ledger
	// namespace is keyed on it.
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	root := initDemo(t)
	seedLiveClaim(t, root, "41", "live-run", now)

	var stderr strings.Builder
	restored, err := restoreInvisibleClaims(
		context.Background(), layoutFor(root), server.newGitHubProvider("token"), repo, now, &stderr)
	if err != nil {
		t.Fatalf("restoreInvisibleClaims: %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1 (stderr = %q)", restored, stderr.String())
	}
	if !server.issueHasLabel(41, providers.LabelClaimed) {
		t.Fatalf("item 41 still carries no %s; a live lease stayed invisible to anyone reading the provider",
			providers.LabelClaimed)
	}
	if !strings.Contains(stderr.String(), "live-run") {
		t.Fatalf("stderr = %q, want the restoring run named", stderr.String())
	}
}

// TestRestoreInvisibleClaimsLeavesVisibleAndDeadClaimsAlone is the guard on
// the ledger staying authoritative: this pass may only make an existing live
// lease visible, never grant, extend, or resurrect one.
func TestRestoreInvisibleClaimsLeavesVisibleAndDeadClaimsAlone(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	// 42 is already visible; 43's lease expired two hours ago.
	server.addIssue(42, "held and visible", providers.LabelClaimed)
	server.addIssue(43, "expired lease")

	t.Setenv("GOOBERS_GAGGLE", "goobers")
	root := initDemo(t)
	seedLiveClaim(t, root, "42", "live-run", now)

	expired, err := localscheduler.OpenClaimLedger(
		filepath.Join(root, "scheduler", claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return now.Add(-2 * time.Hour) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	expiredKey := localscheduler.ClaimKey{
		Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "43",
	}
	if ok, _, err := expired.ClaimScoped(expiredKey, "expired-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed expired claim: ok=%v err=%v", ok, err)
	}

	var stderr strings.Builder
	restored, err := restoreInvisibleClaims(
		context.Background(), layoutFor(root), server.newGitHubProvider("token"), repo, now, &stderr)
	if err != nil {
		t.Fatalf("restoreInvisibleClaims: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0: nothing needed restoring (stderr = %q)", restored, stderr.String())
	}
	if server.issueHasLabel(43, providers.LabelClaimed) {
		t.Fatalf("an expired lease published a claim marker; a dead claim must never resurrect its own marker")
	}
}

// TestRestoreInvisibleClaimsSurvivesAnUnreadableItem keeps the pass
// housekeeping-shaped: it corrects what it can and reports what it cannot,
// rather than letting one unreadable item stop every other correction.
func TestRestoreInvisibleClaimsSurvivesAnUnreadableItem(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	// 44 exists and is invisible; 45 has a lease but no issue behind it.
	server.addIssue(44, "held but invisible")

	t.Setenv("GOOBERS_GAGGLE", "goobers")
	root := initDemo(t)
	seedLiveClaim(t, root, "45", "vanished-run", now)
	seedLiveClaim(t, root, "44", "live-run", now)

	var stderr strings.Builder
	restored, err := restoreInvisibleClaims(
		context.Background(), layoutFor(root), server.newGitHubProvider("token"), repo, now, &stderr)
	if err != nil {
		t.Fatalf("restoreInvisibleClaims: %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1: the readable item must still be corrected", restored)
	}
	if !strings.Contains(stderr.String(), "45") {
		t.Fatalf("stderr = %q, want the unreadable item reported rather than silently skipped", stderr.String())
	}
}

// TestRestoreInvisibleClaimsSkipsAnUnscopedInstance pins the honest outcome for
// a legacy instance whose claims carry no namespace: ListNamespace cannot
// address those keys, so the pass says it is skipping rather than reporting a
// clean check it never performed.
//
// This case is why the gaggle is read from providerGaggle() and not from the
// layout: layoutFor builds a root-only layout for provider stages, so
// l.Gaggle() is ALWAYS empty here. Reading it would have sent every instance
// down this branch — a check that runs on no input, indistinguishable from one
// that found nothing wrong.
func TestRestoreInvisibleClaimsSkipsAnUnscopedInstance(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(46, "held but invisible")

	t.Setenv("GOOBERS_GAGGLE", "")
	root := initDemo(t)
	seedLiveClaim(t, root, "46", "live-run", now)

	var stderr strings.Builder
	restored, err := restoreInvisibleClaims(
		context.Background(), layoutFor(root), server.newGitHubProvider("token"), repo, now, &stderr)
	if err != nil {
		t.Fatalf("restoreInvisibleClaims: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0", restored)
	}
	if !strings.Contains(stderr.String(), "skipping") {
		t.Fatalf("stderr = %q, want the skip stated rather than a silent clean pass", stderr.String())
	}
}
