package readmodel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// QueueEligibilityRow identifies a scoped run's compact queue observation.
type QueueEligibilityRow struct {
	RunID    string
	Evidence QueueEligibilityEvidence
}

// QueueEligibilityReader is the read-only queue projection capability. It is
// separate from Reader so older optional backends can report unavailable
// without falling back to an unbounded journal scan.
type QueueEligibilityReader interface {
	LatestQueueEligibility(context.Context, string, string) (QueueEligibilityRow, bool, error)
}

const latestQueueEligibilitySQL = `SELECT run_id, json_extract(operator_json, '$.QueueEligibility')
FROM run INDEXED BY idx_run_queue_eligibility
WHERE gaggle = ? AND workflow = ? AND json_type(operator_json, '$.QueueEligibility') = 'object'
ORDER BY json_extract(operator_json, '$.QueueEligibility.RecordedAt') DESC, run_id DESC LIMIT 1`

// LatestQueueEligibility returns the newest recorded selection observation in
// exactly one gaggle/workflow scope, including invalid or unavailable reports.
// Scope is applied before LIMIT and the expression index avoids scanning runs
// that never selected PRs. Empty gaggle means legacy unscoped, never all.
func (s *Store) LatestQueueEligibility(ctx context.Context, gaggle, workflow string) (QueueEligibilityRow, bool, error) {
	if workflow == "" {
		return QueueEligibilityRow{}, false, errors.New("readmodel: queue eligibility requires a workflow")
	}
	db, release, err := s.readHandle()
	if err != nil {
		return QueueEligibilityRow{}, false, err
	}
	defer release()
	var row QueueEligibilityRow
	var data []byte
	err = db.QueryRowContext(ctx, latestQueueEligibilitySQL, gaggle, workflow).Scan(&row.RunID, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueEligibilityRow{}, false, nil
	}
	if err != nil {
		return QueueEligibilityRow{}, false, fmt.Errorf("readmodel: queue eligibility: %w", err)
	}
	if err := json.Unmarshal(data, &row.Evidence); err != nil {
		return QueueEligibilityRow{}, false, fmt.Errorf("readmodel: invalid queue evidence: %w", err)
	}
	return row, true, nil
}
