package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestLandingIntentIsNotAnExternalMutation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "intent-only", true: "confirmed"}[confirmed], func(t *testing.T) {
			root := t.TempDir()
			run, err := journal.Create(root, journal.RunIdentity{InstanceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RunID: "landing-intent-run", Workflow: "landing", WorkflowVersion: 1, Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ref := &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "9"}
			intent := &providers.LandingIntent{ID: "0123456789abcdef0123456789abcdef", Operation: "merge", RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", ExpectedHeadSHA: "head"}
			if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: ref, Runner: providers.MutationRunnerFields("merge-intent", nil, nil, intent)}); err != nil {
				t.Fatal(err)
			}
			wantMutations := 0
			if confirmed {
				confirmation := &providers.MergeConfirmation{IntentID: intent.ID, RepositoryAPIURL: intent.RepositoryAPIURL, PullID: intent.PullID, MergeSHA: "merged"}
				if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: ref, Runner: providers.MutationRunnerFields("merge", confirmation, nil, nil)}); err != nil {
					t.Fatal(err)
				}
				wantMutations = 1
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			db := openTestDB(t, t.TempDir())
			if err := db.IngestRun(context.Background(), run.Dir()); err != nil {
				t.Fatal(err)
			}
			var mutations, associations int
			if err := db.sql.QueryRow(`SELECT COUNT(*) FROM provider_mutations`).Scan(&mutations); err != nil || mutations != wantMutations {
				t.Fatalf("mutation count=%d want=%d error=%v", mutations, wantMutations, err)
			}
			if err := db.sql.QueryRow(`SELECT COUNT(*) FROM run_cost_attribution WHERE relationship='merge-intent'`).Scan(&associations); err != nil || associations != 1 {
				t.Fatalf("intent association count=%d error=%v", associations, err)
			}
			query := MergeReportQuery{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)}
			report, err := db.MergeProvenance(context.Background(), query)
			if err != nil || len(report.LandingIntents) != 1 || report.LandingIntents[0].LandingIntent != *intent || len(report.Merges) != wantMutations {
				t.Fatalf("intent query or merge classification wrong: %+v %v", report, err)
			}
			if err := db.IngestRun(context.Background(), run.Dir()); err != nil {
				t.Fatal(err)
			}
			if err := db.DeleteRun(context.Background(), "landing-intent-run"); err != nil {
				t.Fatal(err)
			}
			var retained int
			if err := db.sql.QueryRow(`SELECT COUNT(*) FROM landing_intents`).Scan(&retained); err != nil || retained != 0 {
				t.Fatalf("intent rows outlived run retention: count=%d err=%v", retained, err)
			}
		})
	}
}
