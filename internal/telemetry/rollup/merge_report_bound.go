package rollup

import (
	"context"
	"fmt"
)

// Reject oversized windows before transferring and decoding thousands of JSON
// receipts. Count the same joined rows as the three report readers, without
// display filtering or deduplication, and stop at the first over-budget row.
// Reader-side limits remain necessary: ingestion may advance after this check.
func (db *DB) checkMergeReportEventBound(ctx context.Context, query MergeReportQuery) error {
	var count int
	err := db.readDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT 1 FROM provider_mutations m JOIN runs r ON r.run_id = m.run_id
		WHERE m.kind = 'pr' AND m.operation IN ('merge', 'enqueue')
		AND m.occurred_at >= ? AND m.occurred_at < ?
		UNION ALL
		SELECT 1 FROM landing_intents i JOIN runs r ON r.run_id = i.run_id
		WHERE i.occurred_at >= ? AND i.occurred_at < ?
		LIMIT ?
	)`, formatTime(query.Since), formatTime(query.Until), formatTime(query.Since), formatTime(query.Until), MaxMergeReportEvents+1).Scan(&count)
	if err != nil {
		return fmt.Errorf("bound merge provenance: %w", err)
	}
	if count > MaxMergeReportEvents {
		return fmt.Errorf("merge report exceeds %d retained events; narrow the query scope", MaxMergeReportEvents)
	}
	return nil
}
