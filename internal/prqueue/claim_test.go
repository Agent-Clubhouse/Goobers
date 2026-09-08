package prqueue

import (
	"strings"
	"testing"
	"time"
)

func TestClaimObservationDoesNotInventGlobalAuthority(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	unknown := ObserveClaim(false, "", "current", time.Time{}, now, false)
	if unknown.State != "unknown" || unknown.Comparison != "unavailable" {
		t.Fatalf("unavailable ledger became unclaimed: %+v", unknown)
	}
	labelOnly := ObserveClaim(true, "", "current", time.Time{}, now, true)
	if labelOnly.Comparison != "provider-label-without-live-local-lease" || !strings.Contains(labelOnly.NextStep, "other instances") {
		t.Fatalf("label gained unsafe local authority: %+v", labelOnly)
	}
	for _, owner := range []string{"current", "other"} {
		live := ObserveClaim(true, owner, "current", now.Add(time.Minute), now, false)
		if live.Comparison != "local-lease-without-provider-label" || live.OwnerRunID != owner || !strings.Contains(live.NextStep, "does not invalidate") {
			t.Fatalf("label absence invalidated lease: %+v", live)
		}
	}
	expired := ObserveClaim(true, "other", "current", now, now, true)
	if expired.State != "expired" || expired.Comparison != labelOnly.Comparison {
		t.Fatalf("expiry boundary treated as live: %+v", expired)
	}
}
