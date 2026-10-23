// Package readmodelpg implements the hosted PostgreSQL read model.
package readmodelpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/readmodel"
)

const schemaVersion = 1

var (
	schemaPattern = regexp.MustCompile(`^readmodel_v[1-9][0-9]*$`)
	// ErrStaleOwner means a newer lease epoch has already modified the model.
	ErrStaleOwner = errors.New("readmodelpg: stale owner epoch")
)

// Fence supplies the live lease epoch inside the transaction it fences.
//
// The epoch is intentionally not a constructor argument: callers must obtain it
// from the live lease handle at transaction time, so a process cannot keep
// writing with a startup default after ownership moves.
type Fence interface {
	WithFence(context.Context, func(context.Context, pgx.Tx, int64) error) error
}

// Store is a reader and fenced writer for one completed model version.
type Store struct {
	pool   *pgxpool.Pool
	schema string
	fence  Fence
}

var _ readmodel.Backend = (*Store)(nil)

// New opens the currently active, completed model.
func New(ctx context.Context, pool *pgxpool.Pool, fence Fence) (*Store, error) {
	if fence == nil {
		return nil, errors.New("readmodelpg: nil lease fence")
	}
	var version int
	if err := pool.QueryRow(ctx,
		`SELECT active_version FROM control.readmodel_version WHERE name = 'readmodel'`,
	).Scan(&version); err != nil {
		return nil, fmt.Errorf("readmodelpg: load active version: %w", err)
	}
	return NewVersion(ctx, pool, version, fence)
}

// NewVersion opens a specific completed version. It is used by rebuild
// validation and tests; ordinary readers use New.
func NewVersion(ctx context.Context, pool *pgxpool.Pool, version int, fence Fence) (*Store, error) {
	if fence == nil {
		return nil, errors.New("readmodelpg: nil lease fence")
	}
	schema, err := versionSchema(version)
	if err != nil {
		return nil, err
	}
	var ready bool
	query := fmt.Sprintf(`SELECT ready FROM %s.meta WHERE singleton`, pgx.Identifier{schema}.Sanitize())
	if err := pool.QueryRow(ctx, query).Scan(&ready); err != nil {
		return nil, fmt.Errorf("readmodelpg: open %s: %w", schema, err)
	}
	if !ready {
		return nil, fmt.Errorf("readmodelpg: %s is not ready", schema)
	}
	return &Store{pool: pool, schema: schema, fence: fence}, nil
}

func versionSchema(version int) (string, error) {
	if version < 1 {
		return "", fmt.Errorf("readmodelpg: invalid version %d", version)
	}
	schema := fmt.Sprintf("readmodel_v%d", version)
	if !schemaPattern.MatchString(schema) {
		return "", fmt.Errorf("readmodelpg: invalid schema %q", schema)
	}
	return schema, nil
}

func (s *Store) table(name string) string {
	return pgx.Identifier{s.schema, name}.Sanitize()
}

// State reports the active version's opaque identity and change floor.
func (s *Store) State(ctx context.Context) (readmodel.State, error) {
	var state readmodel.State
	query := fmt.Sprintf(`
		SELECT epoch, min_change_seq, built_at, ready
		FROM %s WHERE singleton`, s.table("meta"))
	if err := s.pool.QueryRow(ctx, query).Scan(
		&state.Epoch, &state.MinChangeSeq, &state.BuiltAt, &state.Ready,
	); err != nil {
		return readmodel.State{}, fmt.Errorf("readmodelpg: state: %w", err)
	}
	state.SchemaVersion = schemaVersion
	return state, nil
}

// GetRun returns a projected run without consulting its journal.
func (s *Store) GetRun(ctx context.Context, runID string) (readmodel.RunRow, bool, error) {
	var raw []byte
	query := fmt.Sprintf(`SELECT projection FROM %s WHERE run_id = $1`, s.table("run"))
	err := s.pool.QueryRow(ctx, query, runID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return readmodel.RunRow{}, false, nil
	}
	if err != nil {
		return readmodel.RunRow{}, false, fmt.Errorf("readmodelpg: get run: %w", err)
	}
	var projection readmodel.Projection
	if err := json.Unmarshal(raw, &projection); err != nil {
		return readmodel.RunRow{}, false, fmt.Errorf("readmodelpg: decode run: %w", err)
	}
	return projection.Run, true, nil
}

// UpsertRun applies a source-monotonic projection and its intake watermark in
// one fenced transaction.
func (s *Store) UpsertRun(ctx context.Context, projection readmodel.Projection) error {
	if projection.Run.RunID == "" {
		return errors.New("readmodelpg: empty run id")
	}
	raw, err := json.Marshal(projection)
	if err != nil {
		return fmt.Errorf("readmodelpg: encode projection: %w", err)
	}
	return s.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if epoch < 1 {
			return fmt.Errorf("readmodelpg: invalid live lease epoch %d", epoch)
		}
		var previousSeq int64
		var previousTerminal bool
		var previousEpoch int64
		query := fmt.Sprintf(`
			SELECT source_seq, (projection->'Run'->>'Terminal')::boolean, owner_epoch
			FROM %s WHERE run_id = $1 FOR UPDATE`, s.table("run"))
		err := tx.QueryRow(ctx, query, projection.Run.RunID).Scan(
			&previousSeq, &previousTerminal, &previousEpoch,
		)
		existed := true
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			existed = false
		case err != nil:
			return fmt.Errorf("readmodelpg: lock run: %w", err)
		case previousEpoch > epoch:
			return ErrStaleOwner
		case projection.Run.LastSeq <= uint64(previousSeq):
			return nil
		}

		if err := s.bumpOwner(ctx, tx, epoch); err != nil {
			return err
		}
		lastActivity := projection.Run.LastActivity
		if lastActivity.IsZero() {
			lastActivity = projection.Run.StartedAt
		}
		stages := projection.Run.Stages
		if stages == nil {
			stages = []string{}
		}
		upsert := fmt.Sprintf(`
			INSERT INTO %s (
				run_id, source_seq, projection, gaggle, workflow, phase, terminal,
				started_at, last_activity_at, disposition, stages,
				any_token_measured, any_premium_measured, any_cost_measured,
				any_retry_waste, owner_epoch
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (run_id) DO UPDATE
			SET source_seq = EXCLUDED.source_seq,
			    projection = EXCLUDED.projection,
			    gaggle = EXCLUDED.gaggle,
			    workflow = EXCLUDED.workflow,
			    phase = EXCLUDED.phase,
			    terminal = EXCLUDED.terminal,
			    started_at = EXCLUDED.started_at,
			    last_activity_at = EXCLUDED.last_activity_at,
			    disposition = EXCLUDED.disposition,
			    stages = EXCLUDED.stages,
			    any_token_measured = EXCLUDED.any_token_measured,
			    any_premium_measured = EXCLUDED.any_premium_measured,
			    any_cost_measured = EXCLUDED.any_cost_measured,
			    any_retry_waste = EXCLUDED.any_retry_waste,
			    owner_epoch = EXCLUDED.owner_epoch
			WHERE %s.owner_epoch <= EXCLUDED.owner_epoch
			  AND %s.source_seq < EXCLUDED.source_seq`,
			s.table("run"), s.table("run"), s.table("run"))
		tag, err := tx.Exec(ctx, upsert,
			projection.Run.RunID, projection.Run.LastSeq, raw,
			projection.Run.Gaggle, projection.Run.Workflow, projection.Run.Phase,
			projection.Run.Terminal, projection.Run.StartedAt, lastActivity,
			projection.Run.Disposition, stages,
			projection.Run.AnyTokenMeasured, projection.Run.AnyPremiumMeasured,
			projection.Run.AnyCostMeasured, projection.Run.AnyRetryWaste, epoch)
		if err != nil {
			return fmt.Errorf("readmodelpg: upsert run: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		watermark := fmt.Sprintf(`
			INSERT INTO %s (run_id, source_seq, owner_epoch)
			VALUES ($1, $2, $3)
			ON CONFLICT (run_id) DO UPDATE
			SET source_seq = EXCLUDED.source_seq,
			    owner_epoch = EXCLUDED.owner_epoch
			WHERE %s.owner_epoch <= EXCLUDED.owner_epoch
			  AND %s.source_seq < EXCLUDED.source_seq`,
			s.table("source_watermark"), s.table("source_watermark"), s.table("source_watermark"))
		if _, err := tx.Exec(ctx, watermark,
			projection.Run.RunID, projection.Run.LastSeq, epoch); err != nil {
			return fmt.Errorf("readmodelpg: advance source watermark: %w", err)
		}
		kind := readmodel.ChangeRunProgressed
		if !existed {
			kind = readmodel.ChangeRunCreated
		} else if !previousTerminal && projection.Run.Terminal {
			kind = readmodel.ChangeRunFinished
		}
		return s.insertChange(ctx, tx, epoch, kind, projection.Run)
	})
}

func (s *Store) bumpOwner(ctx context.Context, tx pgx.Tx, epoch int64) error {
	query := fmt.Sprintf(`
		UPDATE %s SET owner_epoch = $1
		WHERE singleton AND owner_epoch <= $1`, s.table("meta"))
	tag, err := tx.Exec(ctx, query, epoch)
	if err != nil {
		return fmt.Errorf("readmodelpg: claim model epoch: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return nil
}

func (s *Store) insertChange(
	ctx context.Context,
	tx pgx.Tx,
	epoch int64,
	kind readmodel.ChangeKind,
	run readmodel.RunRow,
) error {
	next := fmt.Sprintf(`
		UPDATE %s SET next_change_seq = next_change_seq + 1, owner_epoch = $1
		WHERE singleton AND owner_epoch <= $1
		RETURNING next_change_seq`, s.table("meta"))
	var seq int64
	if err := tx.QueryRow(ctx, next, epoch).Scan(&seq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		return fmt.Errorf("readmodelpg: allocate change sequence: %w", err)
	}
	insert := fmt.Sprintf(`
		INSERT INTO %s (seq, kind, run_id, gaggle, workflow, owner_epoch)
		VALUES ($1, $2, $3, $4, $5, $6)`, s.table("change"))
	if _, err := tx.Exec(ctx, insert, seq, kind, run.RunID, run.Gaggle, run.Workflow, epoch); err != nil {
		return fmt.Errorf("readmodelpg: insert change: %w", err)
	}
	return nil
}

// PruneChanges deletes an acknowledged prefix and advances the floor
// monotonically in the same fenced transaction.
func (s *Store) PruneChanges(ctx context.Context, keepFrom uint64) (int64, error) {
	var pruned int64
	err := s.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if err := s.bumpOwner(ctx, tx, epoch); err != nil {
			return err
		}
		remove := fmt.Sprintf(`
			DELETE FROM %s
			WHERE seq < $1 AND owner_epoch <= $2`, s.table("change"))
		tag, err := tx.Exec(ctx, remove, keepFrom, epoch)
		if err != nil {
			return fmt.Errorf("readmodelpg: prune changes: %w", err)
		}
		pruned = tag.RowsAffected()
		advance := fmt.Sprintf(`
			UPDATE %s
			SET min_change_seq = $1, owner_epoch = $2
			WHERE singleton AND min_change_seq < $1 AND owner_epoch <= $2`, s.table("meta"))
		if _, err := tx.Exec(ctx, advance, keepFrom, epoch); err != nil {
			return fmt.Errorf("readmodelpg: advance change floor: %w", err)
		}
		return nil
	})
	return pruned, err
}

func newEpoch() string {
	return uuid.NewString()
}
