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

func seedMergeReportEvent(t *testing.T, db *DB, n int, instanceID, gaggle, repository, pullID string, confirmed bool, at time.Time) {
	t.Helper()
	runID := fmt.Sprintf("merge-run-%d", n)
	if _, err := db.sql.Exec(`INSERT INTO runs (run_id, workflow, workflow_version, gaggle, started_at, instance_id) VALUES (?, 'landing', 1, ?, ?, ?)`, runID, gaggle, formatTime(at), instanceID); err != nil {
		t.Fatal(err)
	}
	var confirmation *providers.MergeConfirmation
	if confirmed {
		confirmation = &providers.MergeConfirmation{RepositoryAPIURL: repository, PullID: pullID, MergeSHA: "commit"}
	}
	data, err := json.Marshal(providers.MutationRunnerFields("merge", confirmation))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO provider_mutations (run_id, seq, provider, kind, external_id, operation, occurred_at, runner_json) VALUES (?, 1, 'github', 'pr', ?, 'merge', ?, ?)`, runID, pullID, formatTime(at), string(data)); err != nil {
		t.Fatal(err)
	}
}

func TestMergeReportDeduplicatesAndDetectsConflictsBeforeFilters(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	repo := "https://forge.example/repos/acme/app"
	seedMergeReportEvent(t, db, 1, a, "web", repo, "9", true, start)
	seedMergeReportEvent(t, db, 2, a, "web", repo, "9", true, start.Add(time.Minute))
	seedMergeReportEvent(t, db, 3, a, "web", repo, "10", true, start)
	seedMergeReportEvent(t, db, 4, b, "other", repo, "10", true, start)
	seedMergeReportEvent(t, db, 5, "", "web", repo, "11", true, start)
	seedMergeReportEvent(t, db, 6, a, "web", repo, "12", false, start)
	seedMergeReportEvent(t, db, 7, a, "web", "https://forge.example/another/repos/acme/app", "9", true, start)
	seedMergeReportEvent(t, db, 8, a, "web", repo, "13", true, start.Add(24*time.Hour))
	query := MergeReportQuery{Since: start, Until: start.Add(24 * time.Hour), InstanceID: a, Gaggle: "web", RepositoryAPIURL: repo}
	report, err := db.MergeProvenance(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Merges) != 1 || report.Merges[0].PullID != "9" || report.UnverifiedMutationEvents != 2 || report.ConflictingPullRequests != 1 || len(report.Daily) != 1 || report.Daily[0].Count != 1 {
		t.Fatalf("incorrect attribution: %+v", report)
	}
	query.RepositoryAPIURL = ""
	report, err = db.MergeProvenance(context.Background(), query)
	if err != nil || len(report.Merges) != 2 {
		t.Fatalf("repository identity collapsed: %+v, %v", report, err)
	}
}

func TestMergeReportRejectsOversizedOrInvalidWindows(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, query := range []MergeReportQuery{{}, {Since: start, Until: start}, {Since: start, Until: start.Add(91 * 24 * time.Hour)}} {
		if _, err := db.MergeProvenance(context.Background(), query); err == nil {
			t.Fatalf("accepted invalid window: %+v", query)
		}
	}
	seedMergeReportEvent(t, db, 1, strings.Repeat("a", 32), "web", "https://forge.example/repos/acme/app", "9", true, start)
	_, err := db.sql.Exec(`WITH RECURSIVE sequence(n) AS (SELECT 2 UNION ALL SELECT n+1 FROM sequence WHERE n < ?)
		INSERT INTO provider_mutations (run_id, seq, provider, kind, external_id, operation, occurred_at, runner_json)
		SELECT 'merge-run-1', n, 'github', 'pr', '9', 'merge', ?, '{}' FROM sequence`, MaxMergeReportEvents+1, formatTime(start))
	if err != nil {
		t.Fatal(err)
	}
	report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: start, Until: start.Add(time.Hour)})
	if err == nil || !strings.Contains(err.Error(), "narrow") || len(report.Merges) != 0 {
		t.Fatalf("oversized report silently partial: %+v, %v", report, err)
	}
}

func TestMergeReportDetectsConflictingCommitsAfterEmptyReceipt(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, sha := range []string{"", "first", "different"} {
		seedMergeReportEvent(t, db, i, strings.Repeat("a", 32), "web", "https://forge.example/repos/acme/app", "9", true, start.Add(time.Duration(i)*time.Second))
		if _, err := db.sql.Exec(`UPDATE provider_mutations SET runner_json=json_set(runner_json, '$.mergeConfirmation.mergeSha', ?) WHERE run_id=?`, sha, fmt.Sprintf("merge-run-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: start, Until: start.Add(time.Hour)})
	if err != nil || len(report.Merges) != 0 || report.ConflictingPullRequests != 1 {
		t.Fatalf("later conflicting commits ignored: %+v, %v", report, err)
	}
}

func TestMergeReportPreservesOriginalReceiptWhenLaterCommitIsKnown(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, sha := range []string{"", "first", "first", ""} {
		seedMergeReportEvent(t, db, i, strings.Repeat("a", 32), "web", "https://forge.example/repos/acme/app", "9", true, start.Add(time.Duration(i)*time.Second))
		if _, err := db.sql.Exec(`UPDATE provider_mutations SET runner_json=json_set(runner_json, '$.mergeConfirmation.mergeSha', ?) WHERE run_id=?`, sha, fmt.Sprintf("merge-run-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: start, Until: start.Add(time.Hour)})
	if err != nil || len(report.Merges) != 1 || report.ConflictingPullRequests != 0 {
		t.Fatalf("compatible receipts not deduplicated: %+v, %v", report, err)
	}
	merge := report.Merges[0]
	if merge.RunID != "merge-run-0" || merge.MergeSHA != "" || !merge.OccurredAt.Equal(start) {
		t.Fatalf("report synthesized a receipt from separate events: %+v", merge)
	}
}

func TestMergeReportRejectsNoncanonicalAndUntrustedReceipts(t *testing.T) {
	for _, tc := range []struct {
		name, repository, pullID string
	}{
		{"credential", "https://secret@forge.example/repos/acme/app", "9"},
		{"query", "https://forge.example/repos/acme/app?token=secret", "9"},
		{"empty query", "https://forge.example/repos/acme/app?", "9"},
		{"fragment", "https://forge.example/repos/acme/app#other", "9"},
		{"host alias", "https://FORGE.example/repos/acme/app", "9"},
		{"trailing slash", "https://forge.example/repos/acme/app/", "9"},
		{"nondecimal pull", "https://forge.example/repos/acme/app", "nine"},
		{"padded pull", "https://forge.example/repos/acme/app", "09"},
		{"zero pull", "https://forge.example/repos/acme/app", "0"},
		{"negative pull", "https://forge.example/repos/acme/app", "-9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t, t.TempDir())
			start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			seedMergeReportEvent(t, db, 1, strings.Repeat("a", 32), "web", tc.repository, tc.pullID, true, start)
			report, err := db.MergeProvenance(context.Background(), MergeReportQuery{Since: start, Until: start.Add(time.Hour)})
			if err != nil || len(report.Merges) != 0 || report.UnverifiedMutationEvents != 1 {
				t.Fatalf("untrusted receipt counted as verified: %+v, %v", report, err)
			}
		})
	}
}
