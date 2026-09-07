package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// MaxMergeReportEvents bounds the events examined before any display filter.
const MaxMergeReportEvents = 10000

// MergeReportQuery bounds reads by mutation time, not the run's start time.
// Filters apply after conflict detection across the retained window.
type MergeReportQuery struct {
	Since, Until                         time.Time
	InstanceID, Gaggle, RepositoryAPIURL string
}

// ConfirmedMerge is one deduplicated, unambiguous retained landing receipt.
type ConfirmedMerge struct {
	Provider         string    `json:"provider"`
	RepositoryAPIURL string    `json:"repositoryApiUrl"`
	PullID           string    `json:"pullId"`
	MergeSHA         string    `json:"mergeSha,omitempty"`
	InstanceID       string    `json:"instanceId"`
	Gaggle           string    `json:"gaggle"`
	RunID            string    `json:"runId"`
	OccurredAt       time.Time `json:"occurredAt"`
}

// DailyMerges counts unique confirmed PRs in one UTC day and ownership scope.
type DailyMerges struct {
	Day              string `json:"day"`
	InstanceID       string `json:"instanceId"`
	Gaggle           string `json:"gaggle"`
	Provider         string `json:"provider"`
	RepositoryAPIURL string `json:"repositoryApiUrl"`
	Count            int    `json:"count"`
}

// MergeReport is a retained-telemetry report, not a forge inventory. Unknown
// events and conflicts are explicit; neither is silently promoted to verified.
type MergeReport struct {
	Coverage                 string           `json:"coverage"`
	Since                    time.Time        `json:"since"`
	Until                    time.Time        `json:"until"`
	Merges                   []ConfirmedMerge `json:"merges"`
	Daily                    []DailyMerges    `json:"daily"`
	UnverifiedMutationEvents int              `json:"unverifiedMutationEvents"`
	ConflictingPullRequests  int              `json:"conflictingPullRequests"`
	Comparison               *MergeComparison `json:"comparison,omitempty"`
}

type mergeKey struct{ provider, repository, pull string }

type mergeEvidence struct {
	receipt  ConfirmedMerge
	seenSHA  string
	conflict bool
}

func (e *mergeEvidence) observe(merge ConfirmedMerge) {
	if e.receipt.InstanceID != merge.InstanceID || e.receipt.Gaggle != merge.Gaggle || (e.seenSHA != "" && merge.MergeSHA != "" && e.seenSHA != merge.MergeSHA) {
		e.conflict = true
	}
	if e.seenSHA == "" {
		e.seenSHA = merge.MergeSHA
	}
}

// MergeProvenance fails rather than returning a partial count when the bounded
// window contains too many events. No append-only state or journal scan is used.
func (db *DB) MergeProvenance(ctx context.Context, query MergeReportQuery) (MergeReport, error) {
	if query.Since.IsZero() || !query.Until.After(query.Since) || query.Until.Sub(query.Since) > 90*24*time.Hour {
		return MergeReport{}, fmt.Errorf("merge report requires an increasing window of at most 90 days")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := db.readDB().QueryContext(ctx, `SELECT m.provider, m.external_id, m.run_id, m.occurred_at,
		r.gaggle, COALESCE(r.instance_id, ''),
		CASE WHEN length(m.runner_json) <= 16384 THEN m.runner_json ELSE NULL END
		FROM provider_mutations m JOIN runs r ON r.run_id = m.run_id
		WHERE m.kind = 'pr' AND m.operation = 'merge' AND m.occurred_at >= ? AND m.occurred_at < ?
		ORDER BY m.occurred_at, m.run_id, m.seq LIMIT ?`, formatTime(query.Since), formatTime(query.Until), MaxMergeReportEvents+1)
	if err != nil {
		return MergeReport{}, fmt.Errorf("query merge provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := MergeReport{Coverage: "retained-telemetry", Since: query.Since.UTC(), Until: query.Until.UTC(), Merges: []ConfirmedMerge{}, Daily: []DailyMerges{}}
	merges := map[mergeKey]*mergeEvidence{}
	count := 0
	for rows.Next() {
		count++
		if count > MaxMergeReportEvents {
			return MergeReport{}, fmt.Errorf("merge report exceeds %d events; narrow the time window", MaxMergeReportEvents)
		}
		merge, valid, err := scanConfirmedMerge(rows)
		if err != nil {
			return MergeReport{}, err
		}
		if !valid {
			result.UnverifiedMutationEvents++
			continue
		}
		key := mergeKey{merge.Provider, merge.RepositoryAPIURL, merge.PullID}
		if prior, exists := merges[key]; exists {
			prior.observe(merge)
			continue
		}
		merges[key] = &mergeEvidence{receipt: merge, seenSHA: merge.MergeSHA}
	}
	if err := rows.Err(); err != nil {
		return MergeReport{}, err
	}
	for _, evidence := range merges {
		if evidence.conflict {
			result.ConflictingPullRequests++
			continue
		}
		merge := evidence.receipt
		if !matchesMergeQuery(merge, query) {
			continue
		}
		result.Merges = append(result.Merges, merge)
	}
	sort.Slice(result.Merges, func(i, j int) bool {
		a, b := result.Merges[i], result.Merges[j]
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return a.OccurredAt.Before(b.OccurredAt)
		}
		return a.Provider+"\x00"+a.RepositoryAPIURL+"\x00"+a.PullID < b.Provider+"\x00"+b.RepositoryAPIURL+"\x00"+b.PullID
	})
	result.Daily = dailyMergeCounts(result.Merges)
	return result, nil
}

func scanConfirmedMerge(rows *sql.Rows) (ConfirmedMerge, bool, error) {
	var merge ConfirmedMerge
	var timestamp, raw sql.NullString
	if err := rows.Scan(&merge.Provider, &merge.PullID, &merge.RunID, &timestamp, &merge.Gaggle, &merge.InstanceID, &raw); err != nil {
		return merge, false, err
	}
	var err error
	if merge.OccurredAt, err = parseTime(timestamp); err != nil {
		return merge, false, err
	}
	var fields struct {
		Confirmation *providers.MergeConfirmation `json:"mergeConfirmation"`
	}
	if !raw.Valid || json.Unmarshal([]byte(raw.String), &fields) != nil || fields.Confirmation == nil || !instance.ValidIdentity(merge.InstanceID) {
		return merge, false, nil
	}
	c := fields.Confirmation
	if c.PullID != merge.PullID || !canonicalMergePullID(c.PullID) || len(c.MergeSHA) > 128 || !canonicalMergeRepository(c.RepositoryAPIURL) {
		return merge, false, nil
	}
	if merge.Provider != "github" && merge.Provider != "ado" && merge.Provider != "gitea" {
		return merge, false, nil
	}
	merge.RepositoryAPIURL, merge.MergeSHA = c.RepositoryAPIURL, c.MergeSHA
	return merge, true, nil
}

func canonicalMergePullID(id string) bool {
	if len(id) == 0 || len(id) > 32 || id[0] == '0' {
		return false
	}
	for _, digit := range id {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func canonicalMergeRepository(address string) bool {
	if len(address) > 4096 {
		return false
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.Path == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	// Match the producer's normalization, without silently upgrading an
	// untrusted alias into proof. Preserve escaped separators and service paths.
	return u.Host == strings.ToLower(u.Host) && !strings.HasSuffix(u.Path, "/") && u.String() == address
}

func matchesMergeQuery(merge ConfirmedMerge, query MergeReportQuery) bool {
	return (query.InstanceID == "" || query.InstanceID == merge.InstanceID) && (query.Gaggle == "" || query.Gaggle == merge.Gaggle) && (query.RepositoryAPIURL == "" || query.RepositoryAPIURL == merge.RepositoryAPIURL)
}

func dailyMergeCounts(merges []ConfirmedMerge) []DailyMerges {
	counts := map[string]DailyMerges{}
	for _, merge := range merges {
		row := DailyMerges{Day: merge.OccurredAt.UTC().Format("2006-01-02"), InstanceID: merge.InstanceID, Gaggle: merge.Gaggle, Provider: merge.Provider, RepositoryAPIURL: merge.RepositoryAPIURL}
		key := strings.Join([]string{row.Day, row.InstanceID, row.Gaggle, row.Provider, row.RepositoryAPIURL}, "\x00")
		row.Count = counts[key].Count + 1
		counts[key] = row
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]DailyMerges, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, counts[key])
	}
	return rows
}
