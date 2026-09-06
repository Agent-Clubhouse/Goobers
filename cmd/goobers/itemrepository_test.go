package main

import (
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

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
