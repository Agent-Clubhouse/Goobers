package readmodel

import (
	"context"
	"fmt"
	"time"
)

const defaultRemediationExampleLimit = 5000

// RemediationExamples returns projected remediation examples ordered newest-first.
func (s *Store) RemediationExamples(ctx context.Context, limit int) ([]RemediationExampleRow, error) {
	if limit <= 0 {
		limit = defaultRemediationExampleLimit
	}
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, `
		SELECT run_id, stage, attempt, error_class, failure_excerpt, fix_excerpt,
		       did_it_help, observed_at, config_digest
		FROM remediation_example
		ORDER BY observed_at DESC, run_id ASC, stage ASC, attempt ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("readmodel: list remediation examples: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]RemediationExampleRow, 0)
	for rows.Next() {
		var (
			row        RemediationExampleRow
			didItHelp  int
			observedAt string
		)
		if err := rows.Scan(
			&row.RunID, &row.Stage, &row.Attempt, &row.ErrorClass, &row.FailureExcerpt, &row.FixExcerpt,
			&didItHelp, &observedAt, &row.ConfigDigest,
		); err != nil {
			return nil, fmt.Errorf("readmodel: scan remediation example: %w", err)
		}
		parsed, err := time.Parse(timeFormat, observedAt)
		if err != nil {
			return nil, fmt.Errorf("readmodel: parse remediation observed_at %q: %w", observedAt, err)
		}
		row.ObservedAt = parsed
		row.DidItHelp = didItHelp != 0
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: remediation rows: %w", err)
	}
	return out, nil
}
