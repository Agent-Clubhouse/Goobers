package rollup

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/goobers/goobers/internal/runlineage"
)

const continuationPageQuery = `SELECT run_id, status FROM runs
 WHERE trigger_kind = 'manual' AND trigger_ref = ? AND continued_from_run_id = ?
 AND run_id > ? ORDER BY run_id ASC LIMIT ?`

// ContinuationRuns returns a bounded, indexed page. A manual trigger ref alone
// is not continuation identity; old rows remain explicitly incomplete until
// their journals are re-ingested.
func (db *DB) ContinuationRuns(ctx context.Context, sourceRunID, after string) (runlineage.Page, error) {
	var out runlineage.Page
	rows, err := db.readDB().QueryContext(ctx, continuationPageQuery, sourceRunID, sourceRunID, after, runlineage.PageSize+1)
	if err != nil {
		return out, fmt.Errorf("rollup: query continuation runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row runlineage.Run
		var phase sql.NullString
		if err := rows.Scan(&row.ID, &phase); err != nil {
			return out, err
		}
		row.Phase = phase.String
		out.Runs = append(out.Runs, row)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	err = db.readDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs
 WHERE trigger_kind = 'manual' AND trigger_ref = ? AND continued_from_run_id IS NULL)`, sourceRunID).Scan(&out.Incomplete)
	out.Trim()
	return out, err
}
