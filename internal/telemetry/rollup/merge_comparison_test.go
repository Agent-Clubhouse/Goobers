package rollup

import (
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
