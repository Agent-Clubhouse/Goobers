package readmodelpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Builder implements migrate.ReadmodelBuilder. It builds a complete invisible
// version before atomically advancing control.readmodel_version.
type Builder struct {
	pool  *pgxpool.Pool
	fence Fence
}

// NewBuilder constructs a version-and-rebuild adapter.
func NewBuilder(pool *pgxpool.Pool, fence Fence) (*Builder, error) {
	if pool == nil {
		return nil, errors.New("readmodelpg: nil pool")
	}
	if fence == nil {
		return nil, errors.New("readmodelpg: nil lease fence")
	}
	return &Builder{pool: pool, fence: fence}, nil
}

// Build creates and fully populates an inactive version. Repeating Build
// replaces only the inactive target, so an interrupted rebuild is restartable.
func (b *Builder) Build(ctx context.Context, target string) error {
	if !schemaPattern.MatchString(target) {
		return fmt.Errorf("readmodelpg: invalid rebuild target %q", target)
	}
	return b.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		var activeVersion int
		if err := tx.QueryRow(ctx, `
			SELECT active_version FROM control.readmodel_version
			WHERE name = 'readmodel' AND owner_epoch <= $1
			FOR UPDATE`, epoch).Scan(&activeVersion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleOwner
			}
			return fmt.Errorf("readmodelpg: lock active version: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE control.readmodel_version SET owner_epoch = $1
			WHERE name = 'readmodel' AND owner_epoch <= $1`, epoch)
		if err != nil {
			return fmt.Errorf("readmodelpg: claim model pointer: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		active, err := versionSchema(activeVersion)
		if err != nil {
			return err
		}
		if target == active {
			return fmt.Errorf("readmodelpg: refusing to rebuild active schema %s", target)
		}
		lock := fmt.Sprintf(`
			SELECT owner_epoch FROM %s.meta
			WHERE singleton AND owner_epoch <= $1 FOR UPDATE`,
			pgx.Identifier{active}.Sanitize())
		var activeEpoch int64
		if err := tx.QueryRow(ctx, lock, epoch).Scan(&activeEpoch); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleOwner
			}
			return fmt.Errorf("readmodelpg: lock active model: %w", err)
		}
		return createVersion(ctx, tx, target, active, epoch)
	})
}

// CreateEmptyVersion creates a completed empty version without activating it.
// It exists for conformance isolation and disaster-recovery validation.
func (b *Builder) CreateEmptyVersion(ctx context.Context, version int) error {
	target, err := versionSchema(version)
	if err != nil {
		return err
	}
	return b.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		var activeVersion int
		if err := tx.QueryRow(ctx, `
			SELECT active_version FROM control.readmodel_version
			WHERE name = 'readmodel' AND owner_epoch <= $1`, epoch).Scan(&activeVersion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleOwner
			}
			return fmt.Errorf("readmodelpg: load active version: %w", err)
		}
		if activeVersion == version {
			return fmt.Errorf("readmodelpg: refusing to replace active schema %s", target)
		}
		return createVersion(ctx, tx, target, "", epoch)
	})
}

func createVersion(
	ctx context.Context,
	tx pgx.Tx,
	target string,
	source string,
	epoch int64,
) error {
	targetID := pgx.Identifier{target}.Sanitize()
	if _, err := tx.Exec(ctx, `DROP SCHEMA IF EXISTS `+targetID+` CASCADE`); err != nil {
		return fmt.Errorf("readmodelpg: reset rebuild target: %w", err)
	}
	ddl := fmt.Sprintf(`
		CREATE SCHEMA %[1]s;
		CREATE TABLE %[1]s.meta (
			singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
			epoch text NOT NULL,
			min_change_seq bigint NOT NULL DEFAULT 0 CHECK (min_change_seq >= 0),
			next_change_seq bigint NOT NULL DEFAULT 0 CHECK (next_change_seq >= 0),
			built_at timestamptz NOT NULL DEFAULT clock_timestamp(),
			ready boolean NOT NULL DEFAULT false,
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
		);
		CREATE TABLE %[1]s.run (
			run_id text PRIMARY KEY,
			source_seq bigint NOT NULL CHECK (source_seq >= 0),
			projection jsonb NOT NULL,
			gaggle text NOT NULL,
			workflow text NOT NULL,
			phase text NOT NULL,
			terminal boolean NOT NULL,
			started_at timestamptz NOT NULL,
			last_activity_at timestamptz NOT NULL,
			disposition text NOT NULL,
			stages text[] NOT NULL DEFAULT '{}',
			any_token_measured boolean NOT NULL DEFAULT false,
			any_premium_measured boolean NOT NULL DEFAULT false,
			any_cost_measured boolean NOT NULL DEFAULT false,
			any_retry_waste boolean NOT NULL DEFAULT false,
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
		);
		CREATE TABLE %[1]s.change (
			seq bigint PRIMARY KEY CHECK (seq > 0),
			changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
			kind text NOT NULL,
			run_id text NOT NULL,
			gaggle text NOT NULL,
			workflow text NOT NULL,
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
		);
		CREATE TABLE %[1]s.source_watermark (
			run_id text PRIMARY KEY,
			source_seq bigint NOT NULL CHECK (source_seq >= 0),
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
		);
		CREATE INDEX run_recency ON %[1]s.run (started_at DESC, run_id);
		CREATE INDEX run_activity ON %[1]s.run (last_activity_at DESC, run_id);
		CREATE INDEX run_gaggle_recency ON %[1]s.run (gaggle, started_at DESC, run_id);
		CREATE INDEX run_gaggle_workflow_recency ON %[1]s.run (gaggle, workflow, started_at DESC, run_id);
		CREATE INDEX run_phase_recency ON %[1]s.run (phase, started_at DESC, run_id);
		CREATE INDEX run_gaggle_phase_recency ON %[1]s.run (gaggle, phase, started_at DESC, run_id);
		CREATE INDEX run_gaggle_workflow_phase_recency
			ON %[1]s.run (gaggle, workflow, phase, started_at DESC, run_id);
		CREATE INDEX run_stages ON %[1]s.run USING gin (stages);
	`, targetID)
	if _, err := tx.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("readmodelpg: create rebuild target: %w", err)
	}
	meta := fmt.Sprintf(`
		INSERT INTO %s.meta (epoch, ready, owner_epoch)
		VALUES ($1, false, $2)`, targetID)
	if _, err := tx.Exec(ctx, meta, newEpoch(), epoch); err != nil {
		return fmt.Errorf("readmodelpg: initialize rebuild target: %w", err)
	}
	if source != "" {
		if err := copyVersion(ctx, tx, source, target, epoch); err != nil {
			return err
		}
	}
	ready := fmt.Sprintf(`
		UPDATE %s.meta SET ready = true, owner_epoch = $1
		WHERE singleton AND owner_epoch <= $1`, targetID)
	tag, err := tx.Exec(ctx, ready, epoch)
	if err != nil {
		return fmt.Errorf("readmodelpg: complete rebuild target: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return nil
}

func copyVersion(ctx context.Context, tx pgx.Tx, source string, target string, epoch int64) error {
	sourceID := pgx.Identifier{source}.Sanitize()
	targetID := pgx.Identifier{target}.Sanitize()
	statements := []string{
		fmt.Sprintf(`
			INSERT INTO %[1]s.run
			SELECT run_id, source_seq, projection, gaggle, workflow, phase, terminal,
			       started_at, last_activity_at, disposition, stages,
			       any_token_measured, any_premium_measured, any_cost_measured,
			       any_retry_waste, $1
			FROM %[2]s.run`, targetID, sourceID),
		fmt.Sprintf(`
			INSERT INTO %[1]s.change
			SELECT seq, changed_at, kind, run_id, gaggle, workflow, $1
			FROM %[2]s.change`, targetID, sourceID),
		fmt.Sprintf(`
			INSERT INTO %[1]s.source_watermark
			SELECT run_id, source_seq, $1 FROM %[2]s.source_watermark`, targetID, sourceID),
		fmt.Sprintf(`
			UPDATE %[1]s.meta
			SET min_change_seq = source.min_change_seq,
			    next_change_seq = source.next_change_seq,
			    owner_epoch = $1
			FROM %[2]s.meta AS source
			WHERE %[1]s.meta.singleton AND source.singleton
			  AND %[1]s.meta.owner_epoch <= $1`, targetID, sourceID),
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, epoch); err != nil {
			return fmt.Errorf("readmodelpg: copy rebuild version: %w", err)
		}
	}
	return nil
}

// Flip atomically exposes a completed version. The CAS never moves the pointer
// backwards and includes the live lease epoch.
func (b *Builder) Flip(ctx context.Context, name string, version int) error {
	if name != "readmodel" {
		return fmt.Errorf("readmodelpg: unknown model %q", name)
	}
	target, err := versionSchema(version)
	if err != nil {
		return err
	}
	return b.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		var ready bool
		var targetSeq int64
		query := fmt.Sprintf(
			`SELECT ready, next_change_seq FROM %s.meta
			WHERE singleton AND owner_epoch <= $1`,
			pgx.Identifier{target}.Sanitize())
		if err := tx.QueryRow(ctx, query, epoch).Scan(&ready, &targetSeq); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleOwner
			}
			return fmt.Errorf("readmodelpg: validate rebuild target: %w", err)
		}
		if !ready {
			return fmt.Errorf("readmodelpg: rebuild target %s is incomplete", target)
		}
		var current int
		if err := tx.QueryRow(ctx, `
			SELECT active_version FROM control.readmodel_version
			WHERE name = $1 AND owner_epoch <= $2
			FOR UPDATE`, name, epoch).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleOwner
			}
			return fmt.Errorf("readmodelpg: lock model pointer: %w", err)
		}
		if current == version {
			return nil
		}
		if current > version {
			return fmt.Errorf("readmodelpg: active version %d is newer than %d", current, version)
		}
		active, err := versionSchema(current)
		if err != nil {
			return err
		}
		var activeSeq int64
		activeQuery := fmt.Sprintf(
			`SELECT next_change_seq FROM %s.meta WHERE singleton FOR UPDATE`,
			pgx.Identifier{active}.Sanitize())
		if err := tx.QueryRow(ctx, activeQuery).Scan(&activeSeq); err != nil {
			return fmt.Errorf("readmodelpg: lock active model: %w", err)
		}
		if activeSeq != targetSeq {
			return fmt.Errorf(
				"readmodelpg: active model advanced during rebuild (%d to %d)",
				targetSeq, activeSeq)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE control.readmodel_version
			SET active_version = $1, owner_epoch = $2, updated_at = clock_timestamp()
			WHERE name = $3 AND active_version < $1 AND owner_epoch <= $2`,
			version, epoch, name)
		if err != nil {
			return fmt.Errorf("readmodelpg: flip model pointer: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}
