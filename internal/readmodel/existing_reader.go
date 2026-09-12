package readmodel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrExistingProjectionUnavailable means read.db exists but cannot serve this
// build yet (for example, a daemon from the prior version has not rebuilt it).
// One-shot readers may fall back to another derived source for this condition;
// corrupt or unreadable databases remain ordinary errors.
var ErrExistingProjectionUnavailable = errors.New("readmodel: existing projection unavailable")

// ExistingReader exposes only reads and lifecycle management. It cannot build,
// migrate, or write a projection; an absent/incompatible database is an error.
type ExistingReader struct {
	Reader
	QueueEligibilityReader
	FreshnessReporter
	io.Closer
	store *Store
}

// CountOutcomeVerdict returns the number of runs whose terminal business
// outcome matches verdict in the same started-at window used by operator
// summaries. It is intentionally exposed only on the concrete one-shot
// reader: this aggregate serves CLI reconciliation and does not expand the
// portal's closed, backend-neutral Reader query set.
func (r *ExistingReader) CountOutcomeVerdict(ctx context.Context, verdict string, since time.Time) (int, error) {
	if r.store == nil {
		return 0, fmt.Errorf("readmodel: outcome aggregate is unavailable")
	}
	return r.store.CountOutcomeVerdict(ctx, verdict, since)
}

// OpenExistingReader opens the daemon's current projection for one-shot CLI
// reads. mode=ro prevents creation and mutations; unlike immutable=1 it still
// sees committed WAL updates from a running daemon.
func OpenExistingReader(ctx context.Context, path string) (*ExistingReader, error) {
	uri := fileURI(path)
	if uri == "" {
		return nil, fmt.Errorf("readmodel: existing database path is required")
	}
	db, err := sql.Open("sqlite", uri+readerDSNParams)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	store := &Store{reader: db, path: path}
	var schemaVersion int
	if err := db.QueryRowContext(ctx, `SELECT schema_version FROM projection_state WHERE id = 1`).Scan(&schemaVersion); err != nil {
		_ = store.Close()
		if errors.Is(err, sql.ErrNoRows) || isMissingTable(err) {
			return nil, fmt.Errorf("%w: projection state is not initialized", ErrExistingProjectionUnavailable)
		}
		return nil, fmt.Errorf("readmodel: inspect existing schema: %w", err)
	}
	if schemaVersion != len(migrations) {
		_ = store.Close()
		return nil, fmt.Errorf("%w: existing schema %d does not match this build (%d); let the matching daemon rebuild its projection", ErrExistingProjectionUnavailable, schemaVersion, len(migrations))
	}
	if _, err := store.State(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return &ExistingReader{Reader: store, QueueEligibilityReader: store, FreshnessReporter: store, Closer: store, store: store}, nil
}
