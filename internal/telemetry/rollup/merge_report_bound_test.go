package rollup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMergeReportPreflightPreservesExactInclusiveBudget(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedMergeReportEvent(t, db, 1, strings.Repeat("a", 32), "web", "https://forge.example/repos/acme/app", "9", true, at)
	if _, err := db.sql.Exec(`WITH RECURSIVE sequence(n) AS (SELECT 2 UNION ALL SELECT n+1 FROM sequence WHERE n < ?)
		INSERT INTO provider_mutations (run_id,seq,provider,kind,external_id,operation,occurred_at,runner_json)
		SELECT original.run_id,n,provider,kind,external_id,operation,occurred_at,runner_json FROM sequence CROSS JOIN provider_mutations original WHERE original.run_id='merge-run-1' AND original.seq=1`, MaxMergeReportEvents); err != nil {
		t.Fatal(err)
	}
	query := MergeReportQuery{Since: at, Until: at.Add(time.Hour), Gaggle: "filtered-out"}
	if err := db.checkMergeReportEventBound(t.Context(), query); err != nil {
		t.Fatalf("exactly the allowed event budget was rejected: %v", err)
	}
	if _, err := db.sql.Exec(`INSERT INTO landing_intents (run_id,seq,provider,external_id,occurred_at,runner_json) VALUES ('merge-run-1',1,'github','9',?,'{}')`, formatTime(at)); err != nil {
		t.Fatal(err)
	}
	if err := db.checkMergeReportEventBound(t.Context(), query); err == nil || !strings.Contains(err.Error(), "narrow") {
		t.Fatalf("display filter or separate table bypassed the shared bound: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE provider_mutations SET operation='push'`); err != nil {
		t.Fatal(err)
	}
	if err := db.checkMergeReportEventBound(t.Context(), query); err != nil {
		t.Fatalf("non-report mutations consumed report budget: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := db.checkMergeReportEventBound(ctx, query); !errors.Is(err, context.Canceled) {
		t.Fatalf("preflight ignored cancellation: %v", err)
	}
}
