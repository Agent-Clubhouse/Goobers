package rollup

import (
	"context"
	"testing"
	"time"
)

func seedADOCostAttribution(t *testing.T, db *DB, runID, repository, id, itemURL, relationship string) {
	t.Helper()
	if _, err := db.sql.Exec(`
		INSERT INTO run_cost_attribution
			(run_id, provider, repository, external_kind, external_id, url, relationship)
		VALUES (?, 'ado', ?, 'issue', ?, NULLIF(?, ''), ?)`,
		runID, repository, id, itemURL, relationship); err != nil {
		t.Fatalf("insert ado attribution: %v", err)
	}
}

// TestCostAggregatesFoldLegacyUnqualifiedClaimRows pins #6796's read side: an
// already-recorded ADO claim row without repository/url sits in the same run
// beside the qualified rows for the same work item. It must fold into that one
// qualified identity instead of becoming a second aggregate carrying half the
// run's cost.
func TestCostAggregatesFoldLegacyUnqualifiedClaimRows(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer func() { _ = db.Close() }()
	const itemURL = "https://dev.azure.com/org/proj/_workitems/edit/7"
	seedCostRow(t, db, "run-ado", fixtureStart, nil, []costUsage{{nanoAIU: int64Pointer(100)}})
	seedADOCostAttribution(t, db, "run-ado", "", "7", "", "claim")
	seedADOCostAttribution(t, db, "run-ado", "", "7", "", "claim-release")
	seedADOCostAttribution(t, db, "run-ado", "org/proj", "7", itemURL, "link-pr")

	result, err := db.CostAggregates(context.Background(), CostQuery{
		Provider: "ado", ExternalKind: CostExternalKindIssue,
		Since: fixtureStart.Add(-time.Minute), Until: fixtureStart.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 1 {
		t.Fatalf("issues = %+v, want exactly one aggregate for work item 7", result.Issues)
	}
	issue := result.Issues[0]
	if issue.Repository != "org/proj" || issue.URL != itemURL ||
		issue.NanoAIU == nil || *issue.NanoAIU != 100 || issue.TotalRuns != 1 {
		t.Fatalf("issue aggregate = %+v, want org/proj carrying the whole run", issue)
	}
}

// TestCostAggregatesKeepAmbiguousUnqualifiedRowsSeparate is the safety half:
// when one run touched the same numeric id in two qualified repositories, an
// unqualified row cannot be assigned to either, so it is left unqualified
// rather than guessed (#5266).
func TestCostAggregatesKeepAmbiguousUnqualifiedRowsSeparate(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	defer func() { _ = db.Close() }()
	seedCostRow(t, db, "run-ado", fixtureStart, nil, []costUsage{{nanoAIU: int64Pointer(90)}})
	seedADOCostAttribution(t, db, "run-ado", "", "7", "", "claim")
	seedADOCostAttribution(t, db, "run-ado", "org/a", "7", "https://dev.azure.com/org/a/_workitems/edit/7", "status")
	seedADOCostAttribution(t, db, "run-ado", "org/b", "7", "https://dev.azure.com/org/b/_workitems/edit/7", "status")

	result, err := db.CostAggregates(context.Background(), CostQuery{
		Provider: "ado", ExternalKind: CostExternalKindIssue,
		Since: fixtureStart.Add(-time.Minute), Until: fixtureStart.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 3 {
		t.Fatalf("issues = %+v, want the unqualified row kept apart from both repositories", result.Issues)
	}
}
