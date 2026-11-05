package localscheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
)

func TestProviderCleanupCannotReserveOrFinalizePublicationCustody(t *testing.T) {
	for _, id := range []string{"publication-lock/github/owner/repo/42", "merge-lock/publication/github/owner/repo"} {
		t.Run(id, func(t *testing.T) {
			now := time.Now()
			ledger, err := OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"), WithLedgerClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			key := ClaimKey{Gaggle: "g", Provider: "github", ExternalID: id}
			if ok, _, err := ledger.ClaimScoped(key, "run", "publish", time.Minute); err != nil || !ok {
				t.Fatalf("seed: %v %v", ok, err)
			}
			entry, _ := ledger.LookupScoped(key)
			if ok, err := ledger.BeginProviderRelease(entry); err == nil || ok {
				t.Fatal("provider cleanup reserved fixed publication custody")
			}
			current, _ := ledger.LookupScoped(key)
			if current.ReleasePending || !current.ExpiresAt.Equal(entry.ExpiresAt) {
				t.Fatal("rejected cleanup changed custody or expiry")
			}
			// Finalization must independently protect custody even from malformed
			// pending state written by an older or incompatible writer.
			storageKey, _ := key.storageKey()
			current.ReleasePending = true
			ledger.entries[storageKey] = current
			if ok, err := ledger.FinishProviderRelease(entry, claimrelease.Completed.Reason()); err == nil || ok {
				t.Fatal("provider cleanup finalized fixed publication custody")
			}
			current.ReleasePending = false
			ledger.entries[storageKey] = current
			now = now.Add(2 * time.Minute)
			expired, err := ledger.RecoverExpired(now)
			if err != nil || len(expired) != 1 {
				t.Fatalf("rejected reservation blocked fixed expiry: %+v %v", expired, err)
			}
		})
	}
}
