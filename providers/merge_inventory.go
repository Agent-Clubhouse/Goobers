package providers

import "time"

// MergeInventoryRequest selects a half-open merge-time window. Limit bounds
// raw list records, including records excluded from the requested window.
type MergeInventoryRequest struct {
	Repository   RepositoryRef
	Since, Until time.Time
	Limit        int
}

// MergeInventoryEntry describes a forge observation, not daemon provenance.
// MergedBy is the actual merger's provider login; empty means unknown, never
// the PR author or the account configured in the local daemon.
type MergeInventoryEntry struct {
	Provider         ProviderKind `json:"provider"`
	RepositoryAPIURL string       `json:"repositoryApiUrl"`
	PullID           string       `json:"pullId"`
	MergeSHA         string       `json:"mergeSha,omitempty"`
	MergedAt         time.Time    `json:"mergedAt"`
	MergedBy         string       `json:"mergedBy,omitempty"`
}
