package readmodel

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CountOutcomeVerdict counts projected run outcomes in a started-at window.
func (s *Store) CountOutcomeVerdict(ctx context.Context, verdict string, since time.Time) (int, error) {
	verdict = strings.TrimSpace(verdict)
	if verdict == "" {
		return 0, fmt.Errorf("readmodel: outcome verdict is required")
	}
	db, release, err := s.readHandle()
	if err != nil {
		return 0, err
	}
	defer release()

	query := "SELECT COUNT(*) FROM run WHERE outcome_verdict = ?"
	args := []any{verdict}
	if !since.IsZero() {
		query += " AND started_at >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	var count int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("readmodel: count %q outcomes: %w", verdict, err)
	}
	return count, nil
}
