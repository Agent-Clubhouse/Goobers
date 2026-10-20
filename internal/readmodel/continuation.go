package readmodel

import (
	"context"
	"database/sql"

	"github.com/goobers/goobers/internal/runlineage"
)

const continuationPageQuery = `SELECT r.run_id, r.phase FROM run r
 WHERE r.trigger_kind = 'manual' AND r.trigger_ref = ?
 AND json_extract(r.operator_json, '$.ContinuedFromRunID') = r.trigger_ref
 AND r.run_id > ? ORDER BY r.run_id ASC LIMIT ?`

// ContinuationRuns returns one indexed page of authenticated direct children.
func (s *Store) ContinuationRuns(ctx context.Context, sourceRunID, after string) (runlineage.Page, error) {
	var out runlineage.Page
	err := s.withReadRows(ctx, continuationPageQuery, []any{sourceRunID, after, runlineage.PageSize + 1},
		"readmodel: list continuations", "readmodel: list continuations", func(rows *sql.Rows) error {
			var row runlineage.Run
			if err := rows.Scan(&row.ID, &row.Phase); err != nil {
				return err
			}
			out.Runs = append(out.Runs, row)
			return nil
		})
	out.Trim()
	return out, err
}
