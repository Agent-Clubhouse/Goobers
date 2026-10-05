package rollup

import (
	"context"
	"database/sql"
	"fmt"
)

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// queryRows runs a list-style projection query and scans every row with scan.
// A query failure is wrapped with queryErr; scan errors are returned unwrapped.
// The optional iteration error message wraps rows.Err, and an empty result
// stays nil.
func queryRows[T any](
	ctx context.Context,
	q queryer,
	query string,
	args []any,
	queryErr string,
	scan func(*sql.Rows) (T, error),
	iterationErr ...string,
) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", queryErr, err)
	}
	defer func() { _ = rows.Close() }()

	var out []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil && len(iterationErr) > 0 {
		return nil, fmt.Errorf("%s: %w", iterationErr[0], err)
	} else if err != nil {
		return out, err
	}
	return out, nil
}
