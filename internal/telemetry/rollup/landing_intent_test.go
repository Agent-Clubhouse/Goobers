package rollup

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestLandingIntentIsNotAnExternalMutation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "intent-only", true: "confirmed"}[confirmed], func(t *testing.T) {
			root := t.TempDir()
			run, err := journal.Create(root, journal.RunIdentity{RunID: "landing-intent-run", Workflow: "landing", WorkflowVersion: 1, Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ref := &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "9"}
			intent := &providers.LandingIntent{ID: "0123456789abcdef0123456789abcdef", RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", ExpectedHeadSHA: "head"}
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
		})
	}
}
