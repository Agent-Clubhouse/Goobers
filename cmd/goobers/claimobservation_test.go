package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestRestoreClaimVerificationPersistsAndReportsMismatch(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		name := "verified"
		if mismatch {
			name = "ownership-mismatch"
		}

		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("GOOBERS_GAGGLE", "goobers")
			root := initDemo(t)
			now := time.Now()
			seedLiveClaim(t, root, "41", "ledger-owner", now)
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
			server := newFakeGitHubServer(t, repo.Owner, repo.Name)
			server.addIssue(41, "held but invisible")
			if mismatch {
				server.addComment(41, "goobers-claim: run=provider-owner\n\nClaimed elsewhere.")
			} else {
				server.addComment(41, "goobers-claim: run=ledger-owner\n\nClaimed before the label was stripped.")
			}
			var stderr strings.Builder
			_, err := restoreInvisibleClaims(context.Background(), layoutFor(root), server.newGitHubProvider("token"), repo, now, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := ledger.LookupScoped(localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "41"})
			if !ok || entry.RunID != "ledger-owner" || !entry.ExpiresAt.Equal(now.Add(time.Hour)) || entry.Verification.State != name || entry.Verification.ObservedAt.IsZero() {
				t.Fatalf("verification lost or lease mutated: %+v (%s)", entry, stderr.String())
			}
			code, stdout, stderrText := runArgs(t, "claims", "list", "--json", root)
			if code != 0 {
				t.Fatalf("claims list: %d %s", code, stderrText)
			}
			var listed []localscheduler.ClaimEntry
			if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Verification != entry.Verification {
				t.Fatalf("CLI dropped verification: %s", stdout)
			}
			if mismatch {
				data, err := os.ReadFile(mutationsSidecarFile)
				if err != nil {
					t.Fatal(err)
				}
				var fact mutationFact
				if err := json.Unmarshal(data, &fact); err != nil {
					t.Fatal(err)
				}
				if fact.ErrorCode != "provider_ledger_ownership_mismatch" || fact.RunID != "ledger-owner" || fact.ProviderRunID != "provider-owner" || fact.Outcome != "conflict" {
					t.Fatalf("mismatch lacks structured owners: %+v", fact)
				}
				observedAt := entry.Verification.ObservedAt
				// The seeded lease is one hour, longer than DefaultClaimLease.
				// Reconciliation must honor that configured duration.
				now = observedAt.Add(DefaultClaimLease + time.Minute)
				var retryStderr strings.Builder
				if _, err := restoreInvisibleClaims(context.Background(), layoutFor(root), server.newGitHubProvider("token"), repo, now, &retryStderr); err != nil {
					t.Fatal(err)
				}
				ledger, err = localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
				if err != nil {
					t.Fatal(err)
				}
				retriedEntry, _ := ledger.LookupScoped(localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "41"})
				if !retriedEntry.Verification.ObservedAt.Equal(observedAt) || !strings.Contains(retryStderr.String(), "delaying claim-visibility retry") {
					t.Fatalf("recent ownership drift retried immediately: verification=%+v stderr=%q", retriedEntry.Verification, retryStderr.String())
				}
			}
		})
	}
}

func TestRepeatedProviderContentionIsTypedOwnershipDrift(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "claims.json")
	now := time.Now().Add(-3 * time.Hour)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath, localscheduler.WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "7"}
	if ok, _, err := ledger.ClaimScoped(key, "ledger-owner", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed ledger claim: ok=%v err=%v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)
	now = now.Add(time.Minute)
	if ok, err := ledger.RecordClaimVerification(entry, localscheduler.ClaimVerification{
		State: "contended", ObservedAt: now, ProviderRunID: "provider-owner",
	}); err != nil || !ok {
		t.Fatalf("record provider contention: ok=%v err=%v", ok, err)
	}
	if err := ledger.ReleaseScoped(key, "ledger-owner"); err != nil {
		t.Fatal(err)
	}

	client, err := claimsclient.NewFile(claimsclient.FileConfig{LedgerPath: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	session := backlogClaimSession{
		ledger: client, gaggle: "goobers", leaseDuration: time.Hour,
		env: backlogQueryEnv{repo: providers.RepositoryRef{Provider: providers.ProviderGitHub}},
	}
	drift, err := session.repeatedProviderContention(context.Background(), providers.WorkItem{ID: "7"}, "provider-owner")
	if err != nil || drift == nil {
		t.Fatalf("repeated contention = %v, %v; want persistent owner disagreement", drift, err)
	}

	var coded interface{ Code() string }
	if !errors.As(drift, &coded) || coded.Code() != "provider_ledger_ownership_mismatch" ||
		!strings.Contains(drift.Error(), "item 7") || !strings.Contains(drift.Error(), "provider-owner") {
		t.Fatalf("typed drift is not actionable: %v", drift)
	}
}

// leaseLosingProvider runs during on item 7 while this run's provider claim
// marker is being written, then reports that the marker won.
type leaseLosingProvider struct {
	backlogIssueProvider
	during     func(context.Context, providers.ClaimWorkItemRequest) error
	releaseErr error
	released   *[]string
}

func (p leaseLosingProvider) ClaimWorkItem(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.ClaimResult, error) {
	if req.ID == "7" {
		if err := p.during(ctx, req); err != nil {
			return providers.ClaimResult{}, err
		}
	}
	return providers.ClaimResult{Claimed: true, Item: providers.WorkItem{ID: req.ID}}, nil
}

func (p leaseLosingProvider) ReleaseWorkItemClaim(_ context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	*p.released = append(*p.released, req.ID+"@"+req.RunID)
	return providers.WorkItem{ID: req.ID}, p.releaseErr
}

func newLeaseLossSession(t *testing.T, now *time.Time) (*backlogClaimSession, *claimsclient.File, *[]string, *bytes.Buffer) {
	t.Helper()
	ledger, err := claimsclient.NewFile(claimsclient.FileConfig{
		LedgerPath: filepath.Join(t.TempDir(), "claims.json"),
		Options:    []localscheduler.LedgerOption{localscheduler.WithLedgerClock(func() time.Time { return *now })},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	var released []string
	session := &backlogClaimSession{
		env:    backlogQueryEnv{repo: providers.RepositoryRef{Provider: providers.ProviderADO, Project: "example-project"}, stderr: &stderr},
		ledger: ledger, runID: "run-1", workflow: "curate", leaseDuration: time.Hour,
		now: func() time.Time { return *now },
	}
	// Item 8's longer lease outlives the expiry case's clock jump.
	for id, lease := range map[string]time.Duration{"7": time.Hour, "8": 3 * time.Hour} {
		if ok, _, err := ledger.ClaimScoped(t.Context(), claimsclient.Key{ExternalID: id}, "run-1", "curate", lease); err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", id, ok, err)
		}
	}
	for _, id := range []string{"7", "8"} {
		item := providers.WorkItem{ID: id}
		session.claimed = append(session.claimed, item)
		session.newlyClaimed = append(session.newlyClaimed, item)
	}
	return session, ledger, &released, &stderr
}

func stealLease(ledger *claimsclient.File) func(context.Context, providers.ClaimWorkItemRequest) error {
	return func(ctx context.Context, req providers.ClaimWorkItemRequest) error {
		key := claimsclient.Key{ExternalID: req.ID}
		if err := ledger.ReleaseScoped(ctx, key, req.RunID); err != nil {
			return err
		}
		if ok, _, err := ledger.ClaimScoped(ctx, key, "run-other", "curate", time.Hour); err != nil || !ok {
			return errors.Join(err, errors.New("steal refused"))
		}
		return nil
	}
}

func TestConfirmProviderClaimsRetractsMarkerWhenLeaseLostMidClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lose  func(*claimsclient.File, *time.Time) func(context.Context, providers.ClaimWorkItemRequest) error
		other int
	}{
		{name: "replaced", lose: func(l *claimsclient.File, _ *time.Time) func(context.Context, providers.ClaimWorkItemRequest) error {
			return stealLease(l)
		}, other: 1},
		{name: "expired", lose: func(_ *claimsclient.File, now *time.Time) func(context.Context, providers.ClaimWorkItemRequest) error {
			return func(context.Context, providers.ClaimWorkItemRequest) error { *now = now.Add(2 * time.Hour); return nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			session, ledger, released, stderr := newLeaseLossSession(t, &now)
			session.env.issueProvider = leaseLosingProvider{during: tc.lose(ledger, &now), released: released}

			if err := session.confirmProviderClaims(t.Context(), 0); err != nil {
				t.Fatalf("confirmProviderClaims: %v (stderr %q)", err, stderr.String())
			}
			if len(session.claimed) != 1 || session.claimed[0].ID != "8" || len(session.newlyClaimed) != 1 || session.newlyClaimed[0].ID != "8" {
				t.Fatalf("claimed = %+v newlyClaimed = %+v, want only item 8", session.claimed, session.newlyClaimed)
			}
			if len(*released) != 1 || (*released)[0] != "7@run-1" {
				t.Fatalf("released markers = %v, want this run's marker on item 7 retracted", *released)
			}
			if len(session.refusals) != 1 || session.refusals[0].itemID != "7" || session.refusals[0].source != claimRefusalLedger {
				t.Fatalf("refusals = %+v, want item 7 refused by the ledger", session.refusals)
			}
			mine, err := ledger.ForRunAll(t.Context(), "run-1")
			if err != nil || len(mine) != 1 || mine[0].ExternalID != "8" {
				t.Fatalf("run-1 leases = %+v, %v; want only item 8", mine, err)
			}
			if holders, err := ledger.ForRunAll(t.Context(), "run-other"); err != nil || len(holders) != tc.other {
				t.Fatalf("new holder's leases = %+v, %v; want %d untouched", holders, err, tc.other)
			}
		})
	}
}

func TestConfirmProviderClaimsFailsWhenLostLeaseMarkerCannotBeRetracted(t *testing.T) {
	now := time.Now()
	session, ledger, released, _ := newLeaseLossSession(t, &now)
	session.env.issueProvider = leaseLosingProvider{during: stealLease(ledger), released: released, releaseErr: errors.New("provider unavailable")}

	err := session.confirmProviderClaims(t.Context(), 0)
	if err == nil || !strings.Contains(err.Error(), "retract provider claim on item 7") {
		t.Fatalf("confirmProviderClaims = %v, want the failed retraction to fail the stage", err)
	}
	if len(session.claimed) != 2 || len(session.refusals) != 0 {
		t.Fatalf("claimed = %+v refusals = %+v, want item 7 kept for the stage's rollback", session.claimed, session.refusals)
	}
}
