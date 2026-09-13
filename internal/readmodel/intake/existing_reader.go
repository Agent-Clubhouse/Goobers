package intake

import (
	"context"
	"database/sql"
	"fmt"
)

// ExistingReader exposes only freshness reads against an existing intake.db.
// It cannot create, migrate, acknowledge, or write source watermarks.
type ExistingReader struct {
	db *sql.DB
}

// Fence is a stable snapshot of pending work and SQLite's external-commit
// counter for this reader connection.
type Fence struct {
	Pending     int
	DataVersion int64
}

// OpenExistingReader opens intake.db in SQLite read-only mode and verifies its
// schema by executing the bounded count query used by status cutover.
func OpenExistingReader(ctx context.Context, path string) (*ExistingReader, error) {
	db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	reader := &ExistingReader{db: db}
	if _, err := reader.Count(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return reader, nil
}

// Count reports the current pending source-watermark count.
func (r *ExistingReader) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_intake`).Scan(&n); err != nil {
		return 0, fmt.Errorf("intake: count: %w", err)
	}
	return n, nil
}

// Fence returns a count bracketed by an unchanged PRAGMA data_version. This
// detects an enqueue-and-ack commit even when both endpoint counts are zero.
func (r *ExistingReader) Fence(ctx context.Context) (Fence, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var before, after int64
		if err := r.db.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&before); err != nil {
			return Fence{}, fmt.Errorf("intake: read data version: %w", err)
		}
		pending, err := r.Count(ctx)
		if err != nil {
			return Fence{}, err
		}
		if err := r.db.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&after); err != nil {
			return Fence{}, fmt.Errorf("intake: reread data version: %w", err)
		}
		if before == after {
			return Fence{Pending: pending, DataVersion: after}, nil
		}
	}
	return Fence{}, fmt.Errorf("intake: freshness fence did not stabilize")
}

// Close releases the read-only database handle.
func (r *ExistingReader) Close() error { return r.db.Close() }
