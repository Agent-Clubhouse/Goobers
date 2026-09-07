package rollup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestInventoryProofFindsDelayedReceiptsAndOutOfWindowConflicts(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	repository := "https://forge.example/repos/acme/app"
	seedMergeReportEvent(t, db, 1, a, "web", repository, "9", true, start.Add(48*time.Hour))
	seedMergeReportEvent(t, db, 2, b, "other-installation", "https://forge.example/other/repos/acme/app", "9", true, start)
	seedMergeReportEvent(t, db, 3, a, "web", repository, "10", true, start)
	seedMergeReportEvent(t, db, 4, b, "other", repository, "10", true, start.Add(48*time.Hour))
	query := MergeReportQuery{Since: start, Until: start.Add(24 * time.Hour), InstanceID: b, Gaggle: "deliberately-not-a-proof-filter"}
	inventory := []providers.MergeInventoryEntry{
		{Provider: providers.ProviderGitHub, RepositoryAPIURL: repository, PullID: "9", MergedAt: start},
		{Provider: providers.ProviderGitHub, RepositoryAPIURL: repository, PullID: "10", MergedAt: start},
	}
	proof, err := db.MergeProvenanceForInventory(context.Background(), query, inventory)
	if err != nil || len(proof.Merges) != 1 || proof.ConflictingPullRequests != 1 {
		t.Fatalf("delayed proof or conflict missed: %+v %v", proof, err)
	}
	if proof.Merges[0].PullID != "9" || proof.Merges[0].InstanceID != a || !proof.Merges[0].OccurredAt.Equal(start.Add(48*time.Hour)) {
		t.Fatalf("repository isolation or receipt identity lost: %+v", proof)
	}
	empty, err := db.MergeProvenanceForInventory(context.Background(), query, nil)
	if err != nil || len(empty.Merges) != 0 || empty.ConflictingPullRequests != 0 {
		t.Fatalf("empty inventory scanned unrelated receipts: %+v %v", empty, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.MergeProvenanceForInventory(ctx, query, inventory); err == nil {
		t.Fatal("cancelled proof lookup succeeded")
	}
}

func TestInventoryProofIndexUpgradesHistoricalStoreAndServesLookup(t *testing.T) {
	db := openHistoricalTestDB(t, filepath.Join(t.TempDir(), "telemetry.db"), 24)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedMergeReportEvent(t, db, 1, strings.Repeat("a", 32), "web", "https://forge.example/repos/acme/app", "9", true, start)
	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.sql.Query("EXPLAIN QUERY PLAN "+mergeInventoryProofSQL, `[{"provider":"github","pullId":"9"}]`, MaxMergeReportEvents+1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
		if strings.Contains(detail, "SEARCH m USING INDEX idx_provider_mutations_merge_identity") && strings.Contains(detail, "provider=? AND external_id=?") {
			indexed = true
		}
	}
	if err := rows.Err(); err != nil || !indexed {
		t.Fatalf("production proof query does not use upgraded index: indexed=%v err=%v", indexed, err)
	}
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM provider_mutations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("upgrade lost historical receipt: count=%d err=%v", count, err)
	}
}

func TestInventoryProofBoundsAllMatchingRetainedReceipts(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	repository := "https://forge.example/repos/acme/app"
	seedMergeReportEvent(t, db, 1, strings.Repeat("a", 32), "web", repository, "9", true, start.Add(48*time.Hour))
	_, err := db.sql.Exec(`WITH RECURSIVE sequence(n) AS (SELECT 2 UNION ALL SELECT n+1 FROM sequence WHERE n < ?)
		INSERT INTO provider_mutations (run_id, seq, provider, kind, external_id, operation, occurred_at, runner_json)
		SELECT 'merge-run-1', n, 'github', 'pr', '9', 'merge', ?, '{}' FROM sequence`, MaxMergeReportEvents+1, formatTime(start.Add(48*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	proof, err := db.MergeProvenanceForInventory(context.Background(), MergeReportQuery{Since: start, Until: start.Add(time.Hour)}, []providers.MergeInventoryEntry{{Provider: providers.ProviderGitHub, RepositoryAPIURL: repository, PullID: "9", MergedAt: start}})
	if err == nil || len(proof.Merges) != 0 {
		t.Fatalf("oversized retained lookup returned partial proof: %+v %v", proof, err)
	}
}
