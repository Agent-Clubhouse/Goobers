package main

import (
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestItemRepositoryRecordsProviderCompleteRecoveryKey(t *testing.T) {
	layout := instance.NewLayout(initDemo(t))
	repositories := []providers.RepositoryRef{
		{Provider: providers.ProviderGitea, URL: "https://forge-one.example", Owner: "team", Name: "repo", ID: "17"},
		{Provider: providers.ProviderGitea, URL: "https://forge-two.example", Owner: "team", Name: "repo", ID: "17"},
	}
	for i, repo := range repositories {
		runID := []string{"recovery-one", "recovery-two"}[i]
		seedItemRepositoryForTest(t, layout, runID, "7", repo)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["annotation"] == itemRepoAnnotation {
			key, _ := event.Runner["repositoryKey"].(string)
			found[event.RunID] = key
		}
	}
	if found["recovery-one"] != repositories[0].CanonicalKey() || found["recovery-two"] != repositories[1].CanonicalKey() || found["recovery-one"] == found["recovery-two"] {
		t.Fatalf("claim annotations lost recovery repository identity: %v", found)
	}
}

// seedItemRepositoryForTest records itemID's repository identity for runID,
// the fixture-side counterpart of recordItemRepository's production call
// sites (prclaim.go, backlogquery.go, selectsource.go): any test that seeds
// a claim directly against the ledger (bypassing those call sites) must also
// seed this, or claimedItemsForRun's fail-closed check
// (ErrItemRepositoryUnknown) refuses the claim it would otherwise resolve.
func seedItemRepositoryForTest(t *testing.T, l instance.Layout, runID, itemID string, repo providers.RepositoryRef) {
	t.Helper()
	annotations, err := openStageAnnotator(l)
	if err != nil {
		t.Fatalf("openStageAnnotator: %v", err)
	}
	defer func() { _ = annotations.Close() }()
	if err := recordItemRepository(annotations, runID, itemID, itemKindIssue, repo); err != nil {
		t.Fatalf("recordItemRepository: %v", err)
	}
}
