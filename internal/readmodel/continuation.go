package readmodel

import (
	"context"
	"database/sql"
	"strings"
)

// ContinuationRuns returns direct continuations grouped by source run.
func (s *Store) ContinuationRuns(ctx context.Context, sourceRunIDs []string) (map[string][]RunRow, error) {
	out := make(map[string][]RunRow)
	if len(sourceRunIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(sourceRunIDs))
	args := make([]any, len(sourceRunIDs))
	for i, id := range sourceRunIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT ` + runColumns + ` FROM run r
		WHERE r.trigger_kind = 'manual' AND r.trigger_ref IN (` +
		strings.Join(placeholders, ",") + `)
		ORDER BY r.run_id ASC`
	err := s.withReadRows(ctx, query, args,
		"readmodel: list continuations",
		"readmodel: list continuations",
		func(rows *sql.Rows) error {
			row, err := scanRunRow(rows)
			if err != nil {
				return err
			}
			if row.Operator.ContinuedFromRunID != row.TriggerRef {
				return nil
			}
			out[row.TriggerRef] = append(out[row.TriggerRef], row)
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}
