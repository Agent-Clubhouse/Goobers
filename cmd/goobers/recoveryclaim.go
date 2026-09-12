package main

import (
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

// recoveryClaimMatches binds a candidate run to exactly one issue using its
// durable selection-time annotations. Never infer a forge host from current
// configuration or infer an issue from stage output. Legacy annotations lacking
// the provider-complete key cannot authorize automatic restoration.
func recoveryClaimMatches(events []journal.Event, runID, repositoryKey, issueID string) (bool, error) {
	if runID == "" || repositoryKey == "" || issueID == "" {
		return false, fmt.Errorf("recovery claim requires run, repository, and issue identity")
	}
	var selectedKey, selectedID, selectedKind string
	for _, event := range events {
		if event.RunID != runID || event.Type != journal.EventRunnerAnnotation || event.Runner["annotation"] != itemRepoAnnotation {
			continue
		}
		key, keyOK := event.Runner["repositoryKey"].(string)
		id, idOK := event.Runner["itemId"].(string)
		kind, kindOK := event.Runner["kind"].(string)
		if !keyOK || !idOK || !kindOK || key == "" || id == "" || len(key) > 4096 || len(id) > 256 || (kind != itemKindIssue && kind != itemKindPullRequest) {
			return false, ErrItemRepositoryUnknown
		}
		if event.Runner["key"] != itemRepoKey(runID, id) {
			return false, fmt.Errorf("recovery claim annotation key disagrees with its run and item")
		}
		if selectedKey != "" && (key != selectedKey || id != selectedID || kind != selectedKind) {
			return false, fmt.Errorf("recovery snapshot has ambiguous claimed-item history")
		}
		selectedKey, selectedID, selectedKind = key, id, kind
	}
	return selectedKey == repositoryKey && selectedID == issueID && selectedKind == itemKindIssue, nil
}
