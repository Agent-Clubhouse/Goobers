package readservice

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestActiveClaimsFromEntriesKeepsOnlyHeldLeases(t *testing.T) {
	now := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	released := now.Add(-time.Minute)
	entries := []localscheduler.ClaimEntry{
		{ItemID: "young", RunID: "run-young", Workflow: "implement", ClaimedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(time.Hour)},
		{ItemID: "old", Gaggle: "alpha", Provider: "github", RunID: "run-old", Workflow: "curate",
			ClaimedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(time.Hour),
			SharedOwner: sharedclaim.Owner{Instance: "peer-instance", Run: "run-old", Token: "secret-token"}},
		{ItemID: "expired", RunID: "run-expired", ClaimedAt: now.Add(-3 * time.Hour), ExpiresAt: now},
		{ItemID: "released", RunID: "run-released", ClaimedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), ReleasedAt: &released},
		{ItemID: "revoked", RunID: "run-revoked", ClaimedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), SharedRevoked: true},
	}

	claims := ActiveClaimsFromEntries(entries, now)

	if len(claims) != 2 {
		t.Fatalf("claims = %+v, want only the two held leases", claims)
	}
	old, young := claims[0], claims[1]
	if old.ItemID != "old" || young.ItemID != "young" {
		t.Fatalf("order = %s, %s; want oldest first", old.ItemID, young.ItemID)
	}
	if !old.Shared || old.Holder != "peer-instance" || old.AgeSeconds != 7200 || old.Workflow != "curate" || old.RunID != "run-old" {
		t.Fatalf("shared claim = %+v", old)
	}
	if young.Shared || young.Holder != ActiveClaimHolderLocal || young.AgeSeconds != 300 {
		t.Fatalf("local claim = %+v", young)
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-token") {
		t.Fatalf("active claims expose the shared owner token: %s", encoded)
	}
}

// TestWriteActiveClaimsRendersTheCLITable pins `goobers claims active` output
// byte for byte: the table, the empty message, and indented JSON.
func TestWriteActiveClaimsRendersTheCLITable(t *testing.T) {
	now := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	view := ActiveClaimListAt([]localscheduler.ClaimEntry{
		{ItemID: "young", RunID: "run-young", Workflow: "implement", ClaimedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(time.Hour)},
		{ItemID: "old", Gaggle: "alpha", Provider: "github", RunID: "run-old", Workflow: "curate",
			ClaimedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(time.Hour),
			SharedOwner: sharedclaim.Owner{Instance: "peer-instance", Run: "run-old", Token: "secret-token"}},
	}, now)

	var table strings.Builder
	if err := WriteActiveClaims(&table, view, false); err != nil {
		t.Fatal(err)
	}
	wantTable := "ITEM ID\tGAGGLE\tPROVIDER\tWORKFLOW\tRUN ID\tHOLDER\tAGE\n" +
		"old\talpha\tgithub\tcurate\trun-old\tpeer-instance\t2h0m0s\n" +
		"young\t-\t-\timplement\trun-young\tlocal\t5m0s\n"
	if table.String() != wantTable {
		t.Fatalf("table = %q, want %q", table.String(), wantTable)
	}

	var empty strings.Builder
	if err := WriteActiveClaims(&empty, ActiveClaimListAt(nil, now), false); err != nil {
		t.Fatal(err)
	}
	if empty.String() != "no active claims\n" {
		t.Fatalf("empty = %q", empty.String())
	}

	var encoded strings.Builder
	if err := WriteActiveClaims(&encoded, view, true); err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if encoded.String() != string(want)+"\n" {
		t.Fatalf("json = %s, want %s", encoded.String(), want)
	}
}
