package rollup

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestRecoveredReceiptRemainsQueryableWithoutInflatingMerges(t *testing.T) {
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{InstanceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RunID: "recovered-receipt", Workflow: "landing", WorkflowVersion: 1, Gaggle: "web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "9"}
	confirmation := &providers.MergeConfirmation{IntentID: "0123456789abcdef0123456789abcdef", RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", MergeSHA: "merged"}
	for _, kind := range []journal.EventType{journal.EventRunnerMutationRecovered, journal.EventRefTouched} {
		if err := run.Append(journal.Event{Type: kind, ExternalRef: ref, Runner: providers.MutationReceiptRunnerFields("receipt", "merge", confirmation, nil, nil)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Append(journal.Event{Type: journal.EventRunnerMutationRecovered, ExternalRef: ref, Runner: map[string]any{"operation": "merge", "outcome": "failure", "mutationReceiptId": "failed-receipt"}}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, t.TempDir())
	if err := db.IngestRun(t.Context(), run.Dir()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM provider_mutations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("expected both successful evidence rows, excluding failure: %d %v", count, err)
	}
	report, err := db.MergeProvenance(t.Context(), MergeReportQuery{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)})
	if err != nil || len(report.Merges) != 1 {
		t.Fatalf("duplicate custody/projection receipts inflated merges: %+v %v", report, err)
	}
}
