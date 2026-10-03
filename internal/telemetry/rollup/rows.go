package rollup

import (
	"context"
	"database/sql"
	"fmt"
)

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryRows[T any](
	ctx context.Context,
	q queryer,
	query string,
	args []any,
	queryErr string,
	iterErr string,
	scan func(*sql.Rows) (T, error),
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
	if err := rows.Err(); err != nil && iterErr != "" {
		return nil, fmt.Errorf("%s: %w", iterErr, err)
	}
	return out, rows.Err()
}
