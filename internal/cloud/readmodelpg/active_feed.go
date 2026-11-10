package readmodelpg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/readmodel"
)

// ActiveFeedStore resolves control.readmodel_version for every feed operation.
// Writers remain version-pinned and self-fence on a flip; readers must follow
// the pointer so old cursors receive epoch_changed.
type ActiveFeedStore struct{ pool *pgxpool.Pool }

// NewActiveFeedStore constructs an active-version feed reader.
func NewActiveFeedStore(pool *pgxpool.Pool) *ActiveFeedStore {
	return &ActiveFeedStore{pool: pool}
}

func (s *ActiveFeedStore) activeSchema(ctx context.Context) (string, error) {
	var version int
	if err := s.pool.QueryRow(ctx, `
		SELECT active_version FROM control.readmodel_version
		WHERE name='readmodel'`).Scan(&version); err != nil {
		return "", fmt.Errorf("readmodelpg: resolve active feed version: %w", err)
	}
	return versionSchema(version)
}

// State reports the currently active model identity and floor.
func (s *ActiveFeedStore) State(ctx context.Context) (readmodel.State, error) {
	schema, err := s.activeSchema(ctx)
	if err != nil {
		return readmodel.State{}, err
	}
	var state readmodel.State
	query := fmt.Sprintf(`
		SELECT epoch,min_change_seq,built_at,ready
		FROM %s.meta WHERE singleton`, pgx.Identifier{schema}.Sanitize())
	if err := s.pool.QueryRow(ctx, query).Scan(
		&state.Epoch, &state.MinChangeSeq, &state.BuiltAt, &state.Ready,
	); err != nil {
		return readmodel.State{}, fmt.Errorf("readmodelpg: active feed state: %w", err)
	}
	state.SchemaVersion = schemaVersion
	return state, nil
}

// Changes reads the currently active model's retained transitions.
func (s *ActiveFeedStore) Changes(
	ctx context.Context,
	after uint64,
	limit int,
) ([]readmodel.Change, error) {
	schema, err := s.activeSchema(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := fmt.Sprintf(`
		SELECT seq,changed_at,kind,run_id,gaggle,workflow
		FROM %s.change WHERE seq>$1 ORDER BY seq LIMIT $2`,
		pgx.Identifier{schema}.Sanitize())
	rows, err := s.pool.Query(ctx, query, after, limit)
	if err != nil {
		return nil, fmt.Errorf("readmodelpg: active feed changes: %w", err)
	}
	defer rows.Close()
	changes := make([]readmodel.Change, 0, limit)
	for rows.Next() {
		var change readmodel.Change
		if err := rows.Scan(
			&change.Seq, &change.At, &change.Kind, &change.RunID,
			&change.Gaggle, &change.Workflow,
		); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

// LatestChangeSeq returns the active model's committed counter.
func (s *ActiveFeedStore) LatestChangeSeq(ctx context.Context) (uint64, error) {
	schema, err := s.activeSchema(ctx)
	if err != nil {
		return 0, err
	}
	query := fmt.Sprintf(
		`SELECT next_change_seq FROM %s.meta WHERE singleton`,
		pgx.Identifier{schema}.Sanitize())
	var seq uint64
	if err := s.pool.QueryRow(ctx, query).Scan(&seq); err != nil {
		return 0, fmt.Errorf("readmodelpg: active feed head: %w", err)
	}
	return seq, nil
}

var _ readmodel.FeedStore = (*ActiveFeedStore)(nil)
