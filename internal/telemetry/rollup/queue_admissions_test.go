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

func seedQueueReportEvent(t *testing.T, db *DB, n int, instanceID, repo, entryID string, at time.Time) {
	t.Helper()
	seedMergeReportEvent(t, db, n, instanceID, "web", repo, "9", false, at)
	data, err := json.Marshal(providers.MutationRunnerFields("enqueue", nil, &providers.QueueAdmission{RepositoryAPIURL: repo, PullID: "9", EntryID: entryID, ExpectedHeadSHA: "head", EnqueuedAt: at}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE provider_mutations SET operation='enqueue', runner_json=? WHERE run_id=?`, string(data), fmt.Sprintf("merge-run-%d", n)); err != nil {
		t.Fatal(err)
	}
}

func TestMergeReportSeparatesQueueAdmissionsAndConflictsBeforeFilters(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	id := strings.Repeat("a", 32)
	repo := "https://forge.example/repos/acme/app"
	seedQueueReportEvent(t, db, 1, id, repo, "owned", at)
	seedQueueReportEvent(t, db, 2, id, repo, "conflict", at)
	seedQueueReportEvent(t, db, 3, strings.Repeat("b", 32), repo, "conflict", at)
	seedQueueReportEvent(t, db, 4, "", repo, "unknown", at)
	seedQueueReportEvent(t, db, 5, id, repo, "", at)
	seedQueueReportEvent(t, db, 6, id, "https://other.example/repos/acme/app", "owned", at)
	seedQueueReportEvent(t, db, 7, id, repo, "outside-window", at.Add(time.Hour))
	if _, err := db.sql.Exec(`INSERT INTO provider_mutations (run_id,seq,provider,kind,external_id,operation,occurred_at,runner_json)
		SELECT run_id,2,provider,kind,external_id,operation,occurred_at,runner_json FROM provider_mutations WHERE run_id='merge-run-1'`); err != nil {
		t.Fatal(err)
	}
	report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: at, Until: at.Add(time.Hour), InstanceID: id, RepositoryAPIURL: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Merges) != 0 || len(report.Daily) != 0 || len(report.QueueAdmissions) != 1 || report.UnverifiedQueueEvents != 2 || report.ConflictingQueueEntries != 1 {
		t.Fatalf("queue counted as merge or bad ownership: %+v", report)
	}
	entry := report.QueueAdmissions[0]
	if entry.EntryID != "owned" || entry.InstanceID != id || entry.RunID != "merge-run-1" || entry.ExpectedHeadSHA != "head" || !entry.EnqueuedAt.Equal(at) {
		t.Fatalf("lost queue evidence: %+v", entry)
	}
}

func TestMergeReportSharesEventBoundWithQueueAdmissions(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	id := strings.Repeat("a", 32)
	repo := "https://forge.example/repos/acme/app"
	seedQueueReportEvent(t, db, 1, id, repo, "owned", at)
	seedMergeReportEvent(t, db, 2, id, "web", repo, "10", true, at)
	if _, err := db.sql.Exec(`WITH RECURSIVE sequence(n) AS (SELECT 2 UNION ALL SELECT n+1 FROM sequence WHERE n < ?)
		INSERT INTO provider_mutations (run_id,seq,provider,kind,external_id,operation,occurred_at,runner_json)
		SELECT 'merge-run-1',n,'github','pr','9','enqueue',?,'{}' FROM sequence`, MaxMergeReportEvents, formatTime(at)); err != nil {
		t.Fatal(err)
	}
	report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: at, Until: at.Add(time.Hour), Gaggle: "filtered-out"})
	if err == nil || !strings.Contains(err.Error(), "narrow") || len(report.Merges) != 0 || len(report.QueueAdmissions) != 0 {
		t.Fatalf("partial/oversized report: %+v %v", report, err)
	}
}
