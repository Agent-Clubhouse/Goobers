package readmodel

import (
	"context"
	"testing"
	"time"
)

func TestRemediationExamplesReturnsNewestLimitedPage(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	for _, example := range []RemediationExampleRow{
		{RunID: "older", Stage: "implement", Attempt: 1, ObservedAt: base},
		{RunID: "run-b", Stage: "implement", Attempt: 1, ObservedAt: base.Add(time.Hour)},
		{RunID: "run-a", Stage: "implement", Attempt: 1, ObservedAt: base.Add(time.Hour), DidItHelp: true},
	} {
		projection := completed(example.RunID, 1)
		projection.Remediation = []RemediationExampleRow{example}
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.RemediationExamples(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RunID != "run-a" || got[1].RunID != "run-b" {
		t.Fatalf("remediation examples = %+v, want run-a then run-b", got)
	}
	if !got[0].DidItHelp {
		t.Fatal("did_it_help was not decoded")
	}
}
