package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func seedAutoCompleteEvent(t *testing.T, db *DB, n int, instanceID string, intent providers.LandingIntent, at time.Time) {
	t.Helper()
	seedMergeReportEvent(t, db, n, instanceID, "web", intent.RepositoryAPIURL, "9", false, at)
	data, err := json.Marshal(providers.MutationRunnerFields("enqueue", nil, nil, &intent))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE provider_mutations SET provider='ado', operation='enqueue', runner_json=? WHERE run_id=?`, string(data), fmt.Sprintf("merge-run-%d", n)); err != nil {
		t.Fatal(err)
	}
}

func TestAutoCompleteAcknowledgementsAreSeparateAndConflictChecked(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	id := strings.Repeat("a", 32)
	intent := providers.LandingIntent{ID: strings.Repeat("1", 32), Operation: "enqueue", RepositoryAPIURL: "https://dev.azure.com/org/project/_apis/git/repositories/repo", PullID: "9", ExpectedHeadSHA: "head"}
	seedAutoCompleteEvent(t, db, 1, id, intent, at)
	if _, err := db.sql.Exec(`INSERT INTO provider_mutations (run_id,seq,provider,kind,external_id,operation,occurred_at,runner_json)
		SELECT run_id,2,provider,kind,external_id,operation,occurred_at,runner_json FROM provider_mutations WHERE run_id='merge-run-1'`); err != nil {
		t.Fatal(err)
	}
	conflict := intent
	conflict.ID = strings.Repeat("2", 32)
	seedAutoCompleteEvent(t, db, 2, id, conflict, at)
	seedAutoCompleteEvent(t, db, 3, strings.Repeat("b", 32), conflict, at)
	invalid := intent
	invalid.Operation = "merge"
	seedAutoCompleteEvent(t, db, 4, id, invalid, at)
	invalid = intent
	invalid.PullID = "10"
	seedAutoCompleteEvent(t, db, 5, id, invalid, at)
	query := MergeReportQuery{Since: at, Until: at.Add(time.Hour), InstanceID: id}
	report, err := db.MergeProvenance(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Merges) != 0 || len(report.Daily) != 0 || len(report.QueueAdmissions) != 0 || len(report.AutoCompleteAcknowledgements) != 1 || report.ConflictingAutoCompleteIntents != 1 || report.UnverifiedQueueEvents != 2 {
		t.Fatalf("acknowledgements lost/promoted or conflict hidden: %+v", report)
	}
	got := report.AutoCompleteAcknowledgements[0]
	if got.LandingIntent != intent || got.InstanceID != id || got.Gaggle != "web" || got.RunID != "merge-run-1" || got.Provider != "ado" {
		t.Fatalf("acknowledgement identity lost: %+v", got)
	}
	query.RepositoryAPIURL = "https://other.example/repos/acme/app"
	report, err = db.MergeProvenance(context.Background(), query)
	if err != nil || len(report.AutoCompleteAcknowledgements) != 0 || report.ConflictingAutoCompleteIntents != 1 {
		t.Fatalf("filter hid conflict or leaked acknowledgement: %+v %v", report, err)
	}
	if err := db.DeleteRun(context.Background(), "merge-run-1"); err != nil {
		t.Fatal(err)
	}
	query.RepositoryAPIURL = ""
	report, err = db.MergeProvenance(context.Background(), query)
	if err != nil || len(report.AutoCompleteAcknowledgements) != 0 {
		t.Fatalf("retention left deleted acknowledgement: %+v %v", report, err)
	}
}

func TestAutoCompleteAcknowledgementsShareReportBound(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	intent := providers.LandingIntent{ID: strings.Repeat("1", 32), Operation: "enqueue", RepositoryAPIURL: "https://dev.azure.com/org/project/_apis/git/repositories/repo", PullID: "9"}
	seedAutoCompleteEvent(t, db, 1, strings.Repeat("a", 32), intent, at)
	if _, err := db.sql.Exec(`WITH RECURSIVE sequence(n) AS (SELECT 2 UNION ALL SELECT n+1 FROM sequence WHERE n < ?)
		INSERT INTO provider_mutations (run_id,seq,provider,kind,external_id,operation,occurred_at,runner_json)
		SELECT original.run_id,n,provider,kind,external_id,operation,occurred_at,runner_json FROM sequence CROSS JOIN provider_mutations original WHERE original.run_id='merge-run-1' AND original.seq=1`, MaxMergeReportEvents+1); err != nil {
		t.Fatal(err)
	}
	report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: at, Until: at.Add(time.Hour), Gaggle: "filtered-out"})
	if err == nil || !strings.Contains(err.Error(), "narrow") || len(report.AutoCompleteAcknowledgements) != 0 {
		t.Fatalf("deduplication/filter bypassed bound: %+v %v", report, err)
	}
}
