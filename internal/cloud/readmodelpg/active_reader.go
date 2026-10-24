package readmodelpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// ErrReadOnly means a write was attempted through a read-only reader.
var ErrReadOnly = errors.New("readmodelpg: reader is read-only")

type readOnlyFence struct{}

func (readOnlyFence) WithFence(context.Context, func(context.Context, pgx.Tx, int64) error) error {
	return ErrReadOnly
}

// ActiveReader is a read-only readmodel.Reader that follows the active-version
// pointer: after a rebuild flips it, the next call reads the new version instead
// of the one that was active when the reader was opened. Both runtime roles may
// use it (the readmodel_v* inventory rows grant them both SELECT).
type ActiveReader struct {
	pool *pgxpool.Pool
}

var _ readmodel.Reader = (*ActiveReader)(nil)

// NewActiveReader opens a reader, failing now if no model is active and ready.
func NewActiveReader(ctx context.Context, pool *pgxpool.Pool) (*ActiveReader, error) {
	if pool == nil {
		return nil, errors.New("readmodelpg: reader requires a pool")
	}
	reader := &ActiveReader{pool: pool}
	if _, err := reader.store(ctx); err != nil {
		return nil, err
	}
	return reader, nil
}

// store opens the currently active model. Opening is two small reads (the
// pointer and the version's ready flag), so it is done per call rather than
// caching a version that a flip would strand.
func (r *ActiveReader) store(ctx context.Context) (*Store, error) {
	var version int
	if err := r.pool.QueryRow(ctx,
		`SELECT active_version FROM control.readmodel_version WHERE name = 'readmodel'`,
	).Scan(&version); err != nil {
		return nil, fmt.Errorf("readmodelpg: load active version: %w", err)
	}
	return openVersion(ctx, r.pool, version, readOnlyFence{})
}

func withStore[T any](ctx context.Context, r *ActiveReader, read func(*Store) (T, error)) (T, error) {
	store, err := r.store(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	return read(store)
}

// State implements readmodel.Reader.
func (r *ActiveReader) State(ctx context.Context) (readmodel.State, error) {
	return withStore(ctx, r, func(s *Store) (readmodel.State, error) { return s.State(ctx) })
}

// ListRuns implements readmodel.Reader.
func (r *ActiveReader) ListRuns(ctx context.Context, options readmodel.ListOptions) (readmodel.ListPage, error) {
	return withStore(ctx, r, func(s *Store) (readmodel.ListPage, error) { return s.ListRuns(ctx, options) })
}

// LatestPerWorkflow implements readmodel.Reader.
func (r *ActiveReader) LatestPerWorkflow(ctx context.Context, options readmodel.AggregateOptions) ([]readmodel.WorkflowLatest, error) {
	return withStore(ctx, r, func(s *Store) ([]readmodel.WorkflowLatest, error) {
		return s.LatestPerWorkflow(ctx, options)
	})
}

// CreditAssignment implements readmodel.Reader.
func (r *ActiveReader) CreditAssignment(ctx context.Context, options readmodel.CreditOptions) ([]readmodel.NodeCredit, error) {
	return withStore(ctx, r, func(s *Store) ([]readmodel.NodeCredit, error) {
		return s.CreditAssignment(ctx, options)
	})
}

// CausalCredit implements readmodel.Reader.
func (r *ActiveReader) CausalCredit(ctx context.Context, options readmodel.CausalOptions) ([]readmodel.CausalNodeCredit, error) {
	return withStore(ctx, r, func(s *Store) ([]readmodel.CausalNodeCredit, error) {
		return s.CausalCredit(ctx, options)
	})
}

// GetRun implements readmodel.Reader.
func (r *ActiveReader) GetRun(ctx context.Context, runID string) (readmodel.RunRow, bool, error) {
	store, err := r.store(ctx)
	if err != nil {
		return readmodel.RunRow{}, false, err
	}
	return store.GetRun(ctx, runID)
}

// CountByPhase implements readmodel.Reader.
func (r *ActiveReader) CountByPhase(ctx context.Context) (map[journal.RunPhase]int, error) {
	return withStore(ctx, r, func(s *Store) (map[journal.RunPhase]int, error) { return s.CountByPhase(ctx) })
}

// ActiveRunCounts implements readmodel.Reader.
func (r *ActiveReader) ActiveRunCounts(ctx context.Context) ([]readmodel.WorkflowCount, error) {
	return withStore(ctx, r, func(s *Store) ([]readmodel.WorkflowCount, error) { return s.ActiveRunCounts(ctx) })
}

// Changes implements readmodel.Reader.
func (r *ActiveReader) Changes(ctx context.Context, afterSeq uint64, limit int) ([]readmodel.Change, error) {
	return withStore(ctx, r, func(s *Store) ([]readmodel.Change, error) { return s.Changes(ctx, afterSeq, limit) })
}

// LatestChangeSeq implements readmodel.Reader.
func (r *ActiveReader) LatestChangeSeq(ctx context.Context) (uint64, error) {
	return withStore(ctx, r, func(s *Store) (uint64, error) { return s.LatestChangeSeq(ctx) })
}
