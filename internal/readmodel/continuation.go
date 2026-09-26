package readmodel

import (
	"context"
	"fmt"
	"strings"
)

// ContinuationRuns returns direct continuations grouped by source run. The
// trigger reference is the immutable source ID for continuation-created runs.
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
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("readmodel: list continuations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, err := scanRunRow(rows)
		if err != nil {
			return nil, err
		}
		if row.Operator.ContinuedFromRunID != row.TriggerRef {
			continue
		}
		out[row.TriggerRef] = append(out[row.TriggerRef], row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: list continuations: %w", err)
	}
	return out, nil
}
