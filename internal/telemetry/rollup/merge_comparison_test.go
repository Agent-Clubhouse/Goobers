package rollup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestMergeComparisonSeparatesProofFromIdentity(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	repository := "https://api.github.com/repos/acme/app"
	report := MergeReport{Since: start, Until: start.Add(24 * time.Hour), Merges: []ConfirmedMerge{
		{Provider: "github", RepositoryAPIURL: repository, PullID: "1", InstanceID: strings.Repeat("a", 32), Gaggle: "one", MergeSHA: "commit"},
		{Provider: "github", RepositoryAPIURL: repository, PullID: "2", InstanceID: strings.Repeat("b", 32), Gaggle: "two", MergeSHA: "commit"},
		{Provider: "github", RepositoryAPIURL: repository, PullID: "6", InstanceID: strings.Repeat("a", 32), Gaggle: "one", MergeSHA: "contradiction"},
	}}
	var inventory []providers.MergeInventoryEntry
	for i, login := range []string{"SHARED", "shared", "shared", "external", "", "shared"} {
		inventory = append(inventory, providers.MergeInventoryEntry{Provider: providers.ProviderGitHub, RepositoryAPIURL: repository, PullID: string(rune('1' + i)), MergedAt: start.Add(time.Hour), MergedBy: login, MergeSHA: "commit"})
	}
	comparison, err := CompareMergeInventory(report, inventory, []string{"shared"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"daemon-verified", "daemon-verified", "same-identity-unverified", "external", "merger-unknown", "same-identity-unverified"}
	for i, entry := range comparison.Entries {
		if entry.Category != want[i] {
			t.Errorf("PR %s category %q, want %q", entry.PullID, entry.Category, want[i])
		}
		if i >= 2 && (entry.InstanceID != "" || entry.Gaggle != "") {
			t.Errorf("residual invented ownership: %+v", entry)
		}
	}
	if len(comparison.Daily) != 5 {
		t.Fatalf("daily grouping lost fleets or categories: %+v", comparison.Daily)
	}
	for _, row := range comparison.Daily {
		if row.Category == "same-identity-unverified" && row.Count != 2 {
			t.Fatalf("residual count = %d, want 2", row.Count)
		}
	}
	if _, err := CompareMergeInventory(report, append(inventory, inventory[0]), []string{"shared"}); err == nil {
		t.Fatal("duplicate forge entry accepted")
	}
	if _, err := CompareMergeInventory(report, inventory, nil); err == nil {
		t.Fatal("missing shared identity silently classified every actor external")
	}
}

// An accepted queue entry followed by a merged PR is also consistent with
// another actor intervening. Even a matching head/shared identity cannot
// promote the admission into a receipt for the later merge.
func TestMergeComparisonDoesNotCreditLaterMergeToAcceptedQueue(t *testing.T) {
	for _, merger := range []string{"shared", "external", ""} {
		t.Run("merger="+merger, func(t *testing.T) {
			db := openTestDB(t, t.TempDir())
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			repo := "https://api.github.com/repos/acme/app"
			seedQueueReportEvent(t, db, 1, strings.Repeat("a", 32), repo, "accepted-entry", at)
			report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: at, Until: at.Add(time.Hour)})
			if err != nil || len(report.QueueAdmissions) != 1 {
				t.Fatalf("lost actual queue receipt: %+v %v", report, err)
			}
			inventory := []providers.MergeInventoryEntry{{Provider: providers.ProviderGitHub, RepositoryAPIURL: repo, PullID: "9", MergedAt: at.Add(time.Minute), MergedBy: merger, MergeSHA: "head"}}
			comparison, err := CompareMergeInventory(report, inventory, []string{"shared"})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"shared": "same-identity-unverified", "external": "external", "": "merger-unknown"}[merger]
			if len(report.Merges) != 0 || len(report.Daily) != 0 || len(comparison.Entries) != 1 || comparison.Entries[0].Category != want || comparison.Entries[0].InstanceID != "" || comparison.Entries[0].Gaggle != "" {
				t.Fatalf("enqueue promoted to completed merge ownership: %+v %+v", report, comparison)
			}
			if len(comparison.Daily) != 1 || comparison.Daily[0].Category != want || comparison.Daily[0].Count != 1 || comparison.Daily[0].InstanceID != "" {
				t.Fatalf("residual KPI invented ownership: %+v", comparison.Daily)
			}
		})
	}
}
