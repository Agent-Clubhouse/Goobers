package main

import (
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
				now = now.Add(time.Minute)
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
