package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/providers"
)

// MergeProvenanceForInventory resolves all retained receipts for the bounded
// inventory's PRs, regardless of receipt date. This prevents delayed recording
// or a conflict just outside the display window from changing attribution.
// The forge entries, not the receipt timestamps, select the merge-time window.
// It intentionally ignores instance/gaggle/repository display filters.
func (db *DB) MergeProvenanceForInventory(ctx context.Context, query MergeReportQuery, inventory []providers.MergeInventoryEntry) (MergeReport, error) {
	if query.Since.IsZero() || !query.Until.After(query.Since) || query.Until.Sub(query.Since) > 90*24*time.Hour || len(inventory) > MaxMergeReportEvents {
		return MergeReport{}, fmt.Errorf("merge inventory proof requires at most %d PRs and an increasing window of at most 90 days", MaxMergeReportEvents)
	}
	query.InstanceID, query.Gaggle, query.RepositoryAPIURL = "", "", ""
	wanted, rawKeys, err := mergeInventoryProofKeys(query, inventory)
	if err != nil {
		return MergeReport{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// JSON keys avoid a variable-sized SQL statement or host-parameter limit.
	// Match provider and PR before decoding receipts; the scanner then matches
	// the full API repository address, preserving installations on one host.
	rows, err := db.readDB().QueryContext(ctx, mergeInventoryProofSQL, rawKeys, MaxMergeReportEvents+1)
	if err != nil {
		return MergeReport{}, fmt.Errorf("query retained inventory proof: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return readMergeProvenance(rows, query, wanted)
}

const mergeInventoryProofSQL = `SELECT m.provider, m.external_id, m.run_id, m.occurred_at,
		r.gaggle, COALESCE(r.instance_id, ''),
		CASE WHEN length(m.runner_json) <= 16384 THEN m.runner_json ELSE NULL END
		FROM json_each(?) k JOIN provider_mutations m
		ON m.provider=json_extract(k.value, '$.provider') AND m.external_id=json_extract(k.value, '$.pullId')
		JOIN runs r ON r.run_id=m.run_id
		WHERE m.kind='pr' AND m.operation='merge'
		ORDER BY m.occurred_at, m.run_id, m.seq LIMIT ?`

func mergeInventoryProofKeys(query MergeReportQuery, inventory []providers.MergeInventoryEntry) (map[mergeKey]bool, string, error) {
	type lookupKey struct {
		Provider string `json:"provider"`
		PullID   string `json:"pullId"`
	}
	wanted := map[mergeKey]bool{}
	seen := map[lookupKey]bool{}
	keys := []lookupKey{}
	for _, entry := range inventory {
		if entry.Provider != providers.ProviderGitHub && entry.Provider != providers.ProviderADO && entry.Provider != providers.ProviderGitea {
			return nil, "", fmt.Errorf("unsupported merge inventory provider")
		}
		if !canonicalMergeRepository(entry.RepositoryAPIURL) || !canonicalMergePullID(entry.PullID) || entry.MergedAt.Before(query.Since) || !entry.MergedAt.Before(query.Until) {
			return nil, "", fmt.Errorf("invalid or out-of-window merge inventory entry")
		}
		key := mergeKey{string(entry.Provider), entry.RepositoryAPIURL, entry.PullID}
		if wanted[key] {
			return nil, "", fmt.Errorf("duplicate merge inventory entry")
		}
		wanted[key] = true
		lookup := lookupKey{key.provider, key.pull}
		if !seen[lookup] {
			keys = append(keys, lookup)
			seen[lookup] = true
		}
	}
	raw, err := json.Marshal(keys)
	return wanted, string(raw), err
}
