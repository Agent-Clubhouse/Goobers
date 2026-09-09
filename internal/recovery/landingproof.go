package recovery

import (
	"fmt"
	"sort"

	"github.com/goobers/goobers/providers"
)

// LandedHead binds the receiving head to the forge's positive landing receipt.
// Neither an enqueue acknowledgment nor an unpaired landing intent is proof.
type LandedHead struct {
	HeadSHA  string
	MergeSHA string
}

// MatchLandedHeads joins receipts from one trusted run journal by exact intent
// identity. The caller must verify repository identity and journal provenance;
// events from different runs must never be pooled into this input.
func MatchLandedHeads(repositoryAPIURL string, intents []providers.LandingIntent, confirmations []providers.MergeConfirmation) ([]LandedHead, error) {
	if repositoryAPIURL == "" || len(intents) > 4096 || len(confirmations) > 4096 {
		return nil, fmt.Errorf("invalid or oversized recovery landing evidence")
	}
	byID := make(map[string]providers.LandingIntent)
	for _, intent := range intents {
		if intent.ID == "" {
			continue
		}
		if previous, exists := byID[intent.ID]; exists && previous != intent {
			return nil, fmt.Errorf("conflicting recovery landing intent identity")
		}
		byID[intent.ID] = intent
	}
	seen := make(map[string]providers.MergeConfirmation)
	unique := make(map[LandedHead]bool)
	for _, confirmation := range confirmations {
		if confirmation.IntentID == "" {
			continue
		}
		if previous, exists := seen[confirmation.IntentID]; exists && previous != confirmation {
			return nil, fmt.Errorf("conflicting recovery landing confirmation")
		}
		seen[confirmation.IntentID] = confirmation
		intent, exists := byID[confirmation.IntentID]
		if !exists || intent.Operation != "merge" || intent.RepositoryAPIURL != repositoryAPIURL || confirmation.RepositoryAPIURL != repositoryAPIURL || intent.PullID == "" || intent.PullID != confirmation.PullID {
			continue
		}
		if !gitObjectID.MatchString(intent.ExpectedHeadSHA) || !gitObjectID.MatchString(confirmation.MergeSHA) {
			continue
		}
		unique[LandedHead{HeadSHA: intent.ExpectedHeadSHA, MergeSHA: confirmation.MergeSHA}] = true
	}
	heads := make([]LandedHead, 0, len(unique))
	for head := range unique {
		heads = append(heads, head)
	}
	sort.Slice(heads, func(i, j int) bool {
		if heads[i].HeadSHA != heads[j].HeadSHA {
			return heads[i].HeadSHA < heads[j].HeadSHA
		}
		return heads[i].MergeSHA < heads[j].MergeSHA
	})
	return heads, nil
}
