package localscheduler

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
)

func TestProviderReleaseCustodySurvivesExpiryAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	now := time.Now()
	open := func() *ClaimLedger {
		t.Helper()
		l, err := OpenClaimLedger(path, WithLedgerClock(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	l := open()
	if ok, _, err := l.Claim("7", "run", "curation", time.Minute); !ok || err != nil {
		t.Fatalf("claim: %t %v", ok, err)
	}
	entry, _ := l.Lookup("7")
	if ok, err := l.BeginProviderRelease(entry); !ok || err != nil {
		t.Fatalf("reserve: %t %v", ok, err)
	}
	now = now.Add(time.Hour)
	l = open()
	if reaped, err := l.RecoverExpired(now); err != nil || len(reaped) != 0 {
		t.Fatalf("reap discarded cleanup custody: %v %v", reaped, err)
	}
	for _, run := range []string{"run", "successor"} {
		if ok, _, err := l.Claim("7", run, "curation", time.Minute); ok || err != nil {
			t.Fatalf("admitted %s during cleanup: %t %v", run, ok, err)
		}
	}
	if err := l.Release("7", "run", claimrelease.Completed.Reason()); !errors.Is(err, ErrClaimReleasePending) {
		t.Fatalf("ordinary release bypassed cleanup: %v", err)
	}
	if err := l.ForceRelease("7"); !errors.Is(err, ErrClaimReleasePending) {
		t.Fatalf("force release bypassed cleanup: %v", err)
	}
	stale := entry
	stale.ClaimedAt = stale.ClaimedAt.Add(-time.Second)
	if ok, err := l.FinishProviderRelease(stale, claimrelease.Completed.Reason()); ok || err != nil {
		t.Fatalf("stale finalize: %t %v", ok, err)
	}
	if ok, err := l.BeginProviderRelease(entry); !ok || err != nil {
		t.Fatalf("retry reservation: %t %v", ok, err)
	}
	if ok, err := l.FinishProviderRelease(entry, claimrelease.Completed.Reason()); !ok || err != nil {
		t.Fatalf("finalize: %t %v", ok, err)
	}
	if ok, _, err := open().Claim("7", "successor", "implementation", time.Minute); !ok || err != nil {
		t.Fatalf("successor after cleanup: %t %v", ok, err)
	}
}
