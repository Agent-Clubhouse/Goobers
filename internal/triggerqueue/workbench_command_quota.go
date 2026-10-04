package triggerqueue

import (
	"context"
	"database/sql"
)

// workbenchCommandCount includes immutable uncertainty and replay tombstones
// across source command kinds; no kind gets a separate capacity bypass.
func workbenchCommandCount(ctx context.Context, tx *sql.Tx, gaggle string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM workbench_commands WHERE gaggle=?) + (SELECT COUNT(*) FROM needs_human_commands WHERE gaggle=?) + (SELECT COUNT(*) FROM workbench_proposals WHERE gaggle=?) + (SELECT COUNT(*) FROM pr_repair_commands WHERE gaggle=?)`, gaggle, gaggle, gaggle, gaggle).Scan(&count)
	return count, err
}
