package readmodel

import (
	"context"
	"database/sql"
	"fmt"
)

func (s *Store) withReadRows(
	ctx context.Context,
	query string,
	args []any,
	queryErr string,
	iterErr string,
	scan func(*sql.Rows) error,
) error {
	db, release, err := s.readHandle()
	if err != nil {
		return err
	}
	defer release()

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", queryErr, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		if iterErr == "" {
			return err
		}
		return fmt.Errorf("%s: %w", iterErr, err)
	}
	return nil
}
