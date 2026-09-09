package rollup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestLandingIntentQuerySharesReceiptBudget(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedMergeReportEvent(t, db, 1, strings.Repeat("a", 32), "web", "https://forge.example/repos/acme/app", "9", true, start)
	_, err := db.sql.Exec(`WITH RECURSIVE sequence(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM sequence WHERE n < ?)
	 INSERT INTO landing_intents (run_id,seq,provider,external_id,occurred_at,runner_json)
	 SELECT 'merge-run-1', n, 'github', '9', ?, '{}' FROM sequence`, MaxMergeReportEvents, formatTime(start))
	if err != nil {
		t.Fatal(err)
	}
	report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: start, Until: start.Add(time.Hour)})
	if err == nil || !strings.Contains(err.Error(), "narrow") || len(report.Merges) != 0 || len(report.LandingIntents) != 0 {
		t.Fatalf("combined budget silently truncated: %+v %v", report, err)
	}
	if err := db.DeleteRun(context.Background(), "merge-run-1"); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM landing_intents`).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("large intent set survived retention: count=%d err=%v", retained, err)
	}
}

func TestLandingIntentQueryRejectsMalformedRowsAndHonorsScope(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	identity := strings.Repeat("a", 32)
	repository := "https://forge.example/repos/acme/app"
	seedMergeReportEvent(t, db, 1, identity, "web", repository, "9", true, start)
	valid := providers.LandingIntent{ID: strings.Repeat("b", 32), Operation: "enqueue", RepositoryAPIURL: repository, PullID: "9", ExpectedHeadSHA: "head"}
	encode := func(intent providers.LandingIntent) string {
		t.Helper()
		raw, err := json.Marshal(providers.MutationRunnerFields("merge-intent", nil, nil, &intent))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	rows := []string{encode(valid), "{", "null", `{}`, strings.Repeat("x", 16385)}
	for _, mutate := range []func(*providers.LandingIntent){
		func(i *providers.LandingIntent) { i.ID = "unknown" },
		func(i *providers.LandingIntent) { i.Operation = "delete" },
		func(i *providers.LandingIntent) { i.PullID = "10" },
		func(i *providers.LandingIntent) { i.RepositoryAPIURL = "https://secret@forge.example/repos/acme/app" },
		func(i *providers.LandingIntent) { i.ExpectedHeadSHA = strings.Repeat("h", 129) },
	} {
		invalid := valid
		mutate(&invalid)
		rows = append(rows, encode(invalid))
	}
	for n, raw := range rows {
		if _, err := db.sql.Exec(`INSERT INTO landing_intents (run_id,seq,provider,external_id,occurred_at,runner_json) VALUES ('merge-run-1', ?, 'github', '9', ?, ?)`, n+1, formatTime(start), raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []MergeReportQuery{
		{Since: start, Until: start.Add(time.Hour), InstanceID: identity, Gaggle: "web", RepositoryAPIURL: repository},
		{Since: start, Until: start.Add(time.Hour), InstanceID: strings.Repeat("c", 32)},
		{Since: start, Until: start.Add(time.Hour), Gaggle: "other"},
		{Since: start, Until: start.Add(time.Hour), RepositoryAPIURL: "https://forge.example/repos/other/app"},
	} {
		report, err := db.MergeProvenance(context.Background(), query)
		want := 0
		if query.InstanceID == identity {
			want = 1
		}
		if err != nil || len(report.LandingIntents) != want || report.UnverifiedIntentEvents != len(rows)-1 || len(report.Merges) != want {
			t.Fatalf("scope/invalid row handling: query=%+v report=%+v err=%v", query, report, err)
		}
	}
}
