package readmodel

import (
	"context"
	"database/sql"
	"fmt"
	"io"
)

// ExistingReader exposes only reads and lifecycle management. It cannot build,
// migrate, or write a projection; an absent/incompatible database is an error.
type ExistingReader struct {
	Reader
	QueueEligibilityReader
	FreshnessReporter
	io.Closer
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
	state, err := store.State(ctx)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if state.SchemaVersion != len(migrations) {
		_ = store.Close()
		return nil, fmt.Errorf("readmodel: existing schema %d does not match this build (%d); let the matching daemon rebuild its projection", state.SchemaVersion, len(migrations))
	}
	return &ExistingReader{Reader: store, QueueEligibilityReader: store, FreshnessReporter: store, Closer: store}, nil
}
