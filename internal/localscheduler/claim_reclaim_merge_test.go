package localscheduler

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReclaimAllSkipsMergeWindowLeases(t *testing.T) {
	ledger, err := OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	entries := []ClaimEntry{
		{Gaggle: "g", Provider: "github", ExternalID: MergeLockItemIDPrefix + "acme/web"},
		{Gaggle: "g", Provider: "github", ExternalID: "42"},
	}
	ok, holder, err := ledger.ReclaimAll(entries, "run", "implementation", time.Hour)
	if err != nil || !ok || holder != "run" {
		t.Fatalf("reclaim = %v, %q, %v", ok, holder, err)
	}
	claims := ledger.ForRunAll("run")
	if len(claims) != 1 || claims[0].ExternalID != "42" {
		t.Fatalf("reclaimed non-resumable merge window: %+v", claims)
	}
}
