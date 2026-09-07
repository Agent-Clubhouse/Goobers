package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			}
		})
	}
}
