package rollup

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

// MergeComparison separates forge identity from retained daemon evidence.
// Residuals are repository-wide: an unverified merge has no known gaggle.
type MergeComparison struct {
	Coverage string                 `json:"coverage"`
	Entries  []ComparedMerge        `json:"entries"`
	Daily    []MergeComparisonDaily `json:"daily"`
}

// MergeComparisonDaily counts landings by UTC day, category and proven scope.
type MergeComparisonDaily struct {
	Day              string                 `json:"day"`
	Provider         providers.ProviderKind `json:"provider"`
	RepositoryAPIURL string                 `json:"repositoryApiUrl"`
	Category         string                 `json:"category"`
	InstanceID       string                 `json:"instanceId,omitempty"`
	Gaggle           string                 `json:"gaggle,omitempty"`
	Count            int                    `json:"count"`
}

// ComparedMerge classifies one forge-observed landing. Instance and gaggle
// are populated only when retained evidence verifies the landing.
type ComparedMerge struct {
	providers.MergeInventoryEntry
	Category   string `json:"category"`
	InstanceID string `json:"instanceId,omitempty"`
	Gaggle     string `json:"gaggle,omitempty"`
}

// CompareMergeInventory must receive the unfiltered telemetry report so a
// different fleet's proof is not misclassified as a shared-account residual.
// The caller owns obtaining a complete bounded inventory; failures must not
// be replaced with an empty slice. Unknown merger identities stay unknown.
func CompareMergeInventory(report MergeReport, inventory []providers.MergeInventoryEntry, sharedIdentities []string) (MergeComparison, error) {
	if len(inventory) > MaxMergeReportEvents {
		return MergeComparison{}, fmt.Errorf("merge inventory exceeds comparison bound")
	}
	proof := map[mergeKey]ConfirmedMerge{}
	for _, merge := range report.Merges {
		proof[mergeKey{merge.Provider, merge.RepositoryAPIURL, merge.PullID}] = merge
	}
	shared := map[string]bool{}
	for _, identity := range sharedIdentities {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			return MergeComparison{}, fmt.Errorf("shared merge identities cannot be empty")
		}
		shared[strings.ToLower(identity)] = true
	}
	if len(shared) == 0 {
		return MergeComparison{}, fmt.Errorf("at least one shared merge identity is required")
	}
	result := MergeComparison{Coverage: "forge-pagination-with-retained-telemetry", Entries: []ComparedMerge{}}
	seen := map[mergeKey]bool{}
	for _, entry := range inventory {
		key := mergeKey{string(entry.Provider), entry.RepositoryAPIURL, entry.PullID}
		if seen[key] || !canonicalMergePullID(entry.PullID) || !canonicalMergeRepository(entry.RepositoryAPIURL) || entry.MergedAt.Before(report.Since) || !entry.MergedAt.Before(report.Until) {
			return MergeComparison{}, fmt.Errorf("duplicate, invalid, or out-of-window merge inventory entry")
		}
		seen[key] = true
		row := ComparedMerge{MergeInventoryEntry: entry, Category: mergeResidualCategory(entry.MergedBy, shared)}
		if merge, ok := proof[key]; ok && compatibleInventoryCommit(merge, entry) {
			row.Category, row.InstanceID, row.Gaggle = "daemon-verified", merge.InstanceID, merge.Gaggle
		}
		result.Entries = append(result.Entries, row)
	}
	result.Daily = comparisonDaily(result.Entries)
	return result, nil
}

func comparisonDaily(entries []ComparedMerge) []MergeComparisonDaily {
	counts := map[MergeComparisonDaily]int{}
	for _, entry := range entries {
		key := MergeComparisonDaily{Day: MergeComparisonDay(entry), Provider: entry.Provider, RepositoryAPIURL: entry.RepositoryAPIURL, Category: entry.Category, InstanceID: entry.InstanceID, Gaggle: entry.Gaggle}
		counts[key]++
	}
	rows := make([]MergeComparisonDaily, 0, len(counts))
	for key, count := range counts {
		key.Count = count
		rows = append(rows, key)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		return strings.Join([]string{a.Day, string(a.Provider), a.RepositoryAPIURL, a.Category, a.InstanceID, a.Gaggle}, "\x00") < strings.Join([]string{b.Day, string(b.Provider), b.RepositoryAPIURL, b.Category, b.InstanceID, b.Gaggle}, "\x00")
	})
	return rows
}

func compatibleInventoryCommit(merge ConfirmedMerge, entry providers.MergeInventoryEntry) bool {
	// Receipt timestamps may lag the forge's merge time; the commit, when
	// present on both sides, must agree. No author or branch-name inference.
	return merge.MergeSHA == "" || entry.MergeSHA == "" || merge.MergeSHA == entry.MergeSHA
}

func mergeResidualCategory(merger string, shared map[string]bool) string {
	if merger == "" {
		return "merger-unknown"
	}
	if shared[strings.ToLower(merger)] {
		return "same-identity-unverified"
	}
	return "external"
}

// MergeComparisonDay returns the UTC merge day, not the observation day.
func MergeComparisonDay(entry ComparedMerge) string {
	return entry.MergedAt.In(time.UTC).Format("2006-01-02")
}
