package readmodelpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const rebuildBatchSize = 100

// The rebuild copies rows between version schemas with INSERT … SELECT. Each
// copy names its target columns, so a value can never land in a different
// column if a version's column order differs from its source's (#8156); the
// trailing $3 of each SELECT is the copy's owner_epoch.
const (
	runCopyColumns       = "run_id,source_seq,projection,gaggle,workflow,phase,terminal,started_at,last_activity_at,disposition,stages,any_token_measured,any_premium_measured,any_cost_measured,any_retry_waste,owner_epoch"
	stageCopyColumns     = "run_id,stage,run_terminal,had_success,had_failure,had_other,token_measured,premium_measured,cost_measured,retry_waste,owner_epoch"
	watermarkCopyColumns = "run_id,source_seq,owner_epoch"
	changeCopyColumns    = "seq,changed_at,kind,run_id,gaggle,workflow,owner_epoch"
)

// Builder implements migrate.ReadmodelBuilder. Rebuild progress is durable and
// each bounded batch receives its own live fenced transaction.
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

// Build creates or resumes an inactive version. It copies current rows in
// bounded keyset batches, then catches up changes recorded after initialization.
func (b *Builder) Build(ctx context.Context, target string) error {
	if !schemaPattern.MatchString(target) {
		return fmt.Errorf("readmodelpg: invalid rebuild target %q", target)
	}
	sourceVersion, done, err := b.prepare(ctx, target)
	if err != nil || done {
		return err
	}
	for {
		done, err = b.copyBatch(ctx, target, sourceVersion)
		if err != nil || done {
			return err
		}
	}
}

func (b *Builder) prepare(ctx context.Context, target string) (int, bool, error) {
	var sourceVersion int
	var done bool
	err := b.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		var err error
		sourceVersion, err = lockPointer(ctx, tx, epoch)
		if err != nil {
			return err
		}
		source, err := versionSchema(sourceVersion)
		if err != nil {
			return err
		}
		if target == source {
			return fmt.Errorf("readmodelpg: refusing to rebuild active schema %s", target)
		}
		sourceSeq, _, err := lockSourceProgress(ctx, tx, source)
		if err != nil {
			return err
		}
		exists, err := schemaMetaExists(ctx, tx, target)
		if err != nil {
			return err
		}
		if exists {
			var builtFrom *int
			var ready bool
			var targetSeq int64
			query := fmt.Sprintf(`
				SELECT source_version, ready, next_change_seq
				FROM %s.meta WHERE singleton FOR UPDATE`,
				pgx.Identifier{target}.Sanitize())
			if err := tx.QueryRow(ctx, query).Scan(&builtFrom, &ready, &targetSeq); err != nil {
				return fmt.Errorf("readmodelpg: inspect rebuild target: %w", err)
			}
			if builtFrom != nil && *builtFrom == sourceVersion {
				switch {
				case ready && targetSeq == sourceSeq:
					done = true
					return nil
				case ready:
					resume := fmt.Sprintf(`
						UPDATE %s.meta
						SET ready=false, catchup_seq=next_change_seq, owner_epoch=$1
						WHERE singleton AND owner_epoch<=$1`,
						pgx.Identifier{target}.Sanitize())
					tag, err := tx.Exec(ctx, resume, epoch)
					if err != nil {
						return fmt.Errorf("readmodelpg: resume rebuild catch-up: %w", err)
					}
					if tag.RowsAffected() != 1 {
						return ErrStaleOwner
					}
				}
				return nil
			}
		}
		return resetVersion(ctx, tx, target, sourceVersion, sourceSeq, false, epoch)
	})
	return sourceVersion, done, err
}

func (b *Builder) copyBatch(
	ctx context.Context,
	target string,
	sourceVersion int,
) (bool, error) {
	done := false
	err := b.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		activeVersion, err := lockPointer(ctx, tx, epoch)
		if err != nil {
			return err
		}
		if activeVersion != sourceVersion {
			return ErrVersionMismatch
		}
		source, err := versionSchema(sourceVersion)
		if err != nil {
			return err
		}
		targetID := pgx.Identifier{target}.Sanitize()
		var (
			ready        bool
			baseComplete bool
			runCursor    string
			catchupSeq   int64
			builtFrom    *int
		)
		query := fmt.Sprintf(`
			SELECT ready, base_complete, run_cursor, catchup_seq, source_version
			FROM %s.meta
			WHERE singleton AND owner_epoch<=$1
			FOR UPDATE`, targetID)
		if err := tx.QueryRow(ctx, query, epoch).Scan(
			&ready, &baseComplete, &runCursor, &catchupSeq, &builtFrom,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleOwner
			}
			return fmt.Errorf("readmodelpg: lock rebuild progress: %w", err)
		}
		if builtFrom == nil || *builtFrom != sourceVersion {
			return ErrVersionMismatch
		}
		sourceSeq, sourceFloor, err := lockSourceProgress(ctx, tx, source)
		if err != nil {
			return err
		}
		if ready && catchupSeq == sourceSeq {
			done = true
			return nil
		}
		if !baseComplete {
			return copyRunBatch(ctx, tx, source, target, runCursor, epoch)
		}
		if sourceFloor > catchupSeq+1 {
			if err := truncateVersion(ctx, tx, target); err != nil {
				return err
			}
			restart := fmt.Sprintf(`
				UPDATE %s.meta
				SET run_cursor='', base_complete=false, catchup_seq=$1,
				    min_change_seq=$1+1, next_change_seq=$1,
				    ready=false, owner_epoch=$2
				WHERE singleton AND owner_epoch<=$2`, targetID)
			tag, err := tx.Exec(ctx, restart, sourceSeq, epoch)
			if err != nil {
				return fmt.Errorf("readmodelpg: restart pruned rebuild: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return ErrStaleOwner
			}
			return nil
		}
		if catchupSeq < sourceSeq {
			copiedThrough, err := copyChangeBatch(
				ctx, tx, source, target, catchupSeq, sourceSeq, epoch)
			if err != nil {
				return err
			}
			catchupSeq = copiedThrough
		}
		if catchupSeq == sourceSeq {
			complete := fmt.Sprintf(`
				UPDATE %s.meta
				SET ready=true, min_change_seq=GREATEST(min_change_seq,$1),
				    next_change_seq=$2,
				    catchup_seq=$2, owner_epoch=$3
				WHERE singleton AND owner_epoch<=$3`, targetID)
			tag, err := tx.Exec(ctx, complete, sourceFloor, sourceSeq, epoch)
			if err != nil {
				return fmt.Errorf("readmodelpg: complete rebuild: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return ErrStaleOwner
			}
			done = true
		}
		return nil
	})
	return done, err
}

func copyRunBatch(
	ctx context.Context,
	tx pgx.Tx,
	source string,
	target string,
	after string,
	epoch int64,
) error {
	sourceID := pgx.Identifier{source}.Sanitize()
	targetID := pgx.Identifier{target}.Sanitize()
	var last string
	var count int
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
		SELECT COALESCE(max(run_id),''), count(*)
		FROM (
			SELECT run_id FROM %s.run
			WHERE run_id>$1 ORDER BY run_id LIMIT $2
		) AS batch`, sourceID), after, rebuildBatchSize).Scan(&last, &count); err != nil {
		return fmt.Errorf("readmodelpg: select rebuild batch: %w", err)
	}
	if count == 0 {
		query := fmt.Sprintf(`
			UPDATE %s.meta SET base_complete=true, owner_epoch=$1
			WHERE singleton AND owner_epoch<=$1`, targetID)
		tag, err := tx.Exec(ctx, query, epoch)
		if err != nil {
			return fmt.Errorf("readmodelpg: finish base copy: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	}
	runCopy := fmt.Sprintf(`
		INSERT INTO %[1]s.run (`+runCopyColumns+`)
		SELECT run_id,source_seq,projection,gaggle,workflow,phase,terminal,
		       started_at,last_activity_at,disposition,stages,
		       any_token_measured,any_premium_measured,any_cost_measured,
		       any_retry_waste,$3
		FROM %[2]s.run
		WHERE run_id>$1 AND run_id<=$2
		ON CONFLICT (run_id) DO UPDATE
		SET source_seq=excluded.source_seq,projection=excluded.projection,
		    gaggle=excluded.gaggle,workflow=excluded.workflow,phase=excluded.phase,
		    terminal=excluded.terminal,started_at=excluded.started_at,
		    last_activity_at=excluded.last_activity_at,disposition=excluded.disposition,
		    stages=excluded.stages,any_token_measured=excluded.any_token_measured,
		    any_premium_measured=excluded.any_premium_measured,
		    any_cost_measured=excluded.any_cost_measured,
		    any_retry_waste=excluded.any_retry_waste,owner_epoch=excluded.owner_epoch
		WHERE %[1]s.run.owner_epoch<=excluded.owner_epoch`, targetID, sourceID)
	if _, err := tx.Exec(ctx, runCopy, after, last, epoch); err != nil {
		return fmt.Errorf("readmodelpg: copy run batch: %w", err)
	}
	stageCopy := fmt.Sprintf(`
		INSERT INTO %[1]s.run_stage (`+stageCopyColumns+`)
		SELECT run_id,stage,run_terminal,had_success,had_failure,had_other,
		       token_measured,premium_measured,cost_measured,retry_waste,$3
		FROM %[2]s.run_stage WHERE run_id>$1 AND run_id<=$2
		ON CONFLICT (run_id,stage) DO UPDATE
		SET run_terminal=excluded.run_terminal,had_success=excluded.had_success,
		    had_failure=excluded.had_failure,had_other=excluded.had_other,
		    token_measured=excluded.token_measured,
		    premium_measured=excluded.premium_measured,
		    cost_measured=excluded.cost_measured,retry_waste=excluded.retry_waste,
		    owner_epoch=excluded.owner_epoch
		WHERE %[1]s.run_stage.owner_epoch<=excluded.owner_epoch`, targetID, sourceID)
	if _, err := tx.Exec(ctx, stageCopy, after, last, epoch); err != nil {
		return fmt.Errorf("readmodelpg: copy stage batch: %w", err)
	}
	watermarkCopy := fmt.Sprintf(`
		INSERT INTO %[1]s.source_watermark (`+watermarkCopyColumns+`)
		SELECT run_id,source_seq,$3 FROM %[2]s.source_watermark
		WHERE run_id>$1 AND run_id<=$2
		ON CONFLICT (run_id) DO UPDATE
		SET source_seq=excluded.source_seq,owner_epoch=excluded.owner_epoch
		WHERE %[1]s.source_watermark.owner_epoch<=excluded.owner_epoch`, targetID, sourceID)
	if _, err := tx.Exec(ctx, watermarkCopy, after, last, epoch); err != nil {
		return fmt.Errorf("readmodelpg: copy watermark batch: %w", err)
	}
	progress := fmt.Sprintf(`
		UPDATE %s.meta SET run_cursor=$1, owner_epoch=$2
		WHERE singleton AND owner_epoch<=$2`, targetID)
	tag, err := tx.Exec(ctx, progress, last, epoch)
	if err != nil {
		return fmt.Errorf("readmodelpg: advance run copy: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleOwner
	}
	return nil
}

func copyChangeBatch(
	ctx context.Context,
	tx pgx.Tx,
	source string,
	target string,
	after int64,
	through int64,
	epoch int64,
) (int64, error) {
	sourceID := pgx.Identifier{source}.Sanitize()
	targetID := pgx.Identifier{target}.Sanitize()
	var last int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
		SELECT COALESCE(max(seq),$1) FROM (
			SELECT seq FROM %s.change
			WHERE seq>$1 AND seq<=$2 ORDER BY seq LIMIT $3
		) AS batch`, sourceID), after, through, rebuildBatchSize).Scan(&last); err != nil {
		return 0, fmt.Errorf("readmodelpg: select change batch: %w", err)
	}
	if last == after {
		return after, fmt.Errorf(
			"readmodelpg: change feed gap after %d through %d", after, through)
	}
	changes := fmt.Sprintf(`
		INSERT INTO %[1]s.change (`+changeCopyColumns+`)
		SELECT seq,changed_at,kind,run_id,gaggle,workflow,$3
		FROM %[2]s.change WHERE seq>$1 AND seq<=$2
		ON CONFLICT (seq) DO NOTHING`, targetID, sourceID)
	if _, err := tx.Exec(ctx, changes, after, last, epoch); err != nil {
		return 0, fmt.Errorf("readmodelpg: copy change batch: %w", err)
	}
	changedRuns := fmt.Sprintf(
		`SELECT DISTINCT run_id FROM %s.change WHERE seq>$1 AND seq<=$2 AND run_id<>''`,
		sourceID)
	runRefresh := fmt.Sprintf(`
		INSERT INTO %[1]s.run (`+runCopyColumns+`)
		SELECT r.run_id,r.source_seq,r.projection,r.gaggle,r.workflow,r.phase,r.terminal,
		       r.started_at,r.last_activity_at,r.disposition,r.stages,
		       r.any_token_measured,r.any_premium_measured,r.any_cost_measured,
		       r.any_retry_waste,$3
		FROM %[2]s.run AS r JOIN (%[3]s) AS changed USING (run_id)
		ON CONFLICT (run_id) DO UPDATE
		SET source_seq=excluded.source_seq,projection=excluded.projection,
		    gaggle=excluded.gaggle,workflow=excluded.workflow,phase=excluded.phase,
		    terminal=excluded.terminal,started_at=excluded.started_at,
		    last_activity_at=excluded.last_activity_at,disposition=excluded.disposition,
		    stages=excluded.stages,any_token_measured=excluded.any_token_measured,
		    any_premium_measured=excluded.any_premium_measured,
		    any_cost_measured=excluded.any_cost_measured,
		    any_retry_waste=excluded.any_retry_waste,owner_epoch=excluded.owner_epoch
		WHERE %[1]s.run.owner_epoch<=excluded.owner_epoch`,
		targetID, sourceID, changedRuns)
	if _, err := tx.Exec(ctx, runRefresh, after, last, epoch); err != nil {
		return 0, fmt.Errorf("readmodelpg: refresh changed runs: %w", err)
	}
	deleteStages := fmt.Sprintf(`
		DELETE FROM %[1]s.run_stage AS target
		USING (%[2]s) AS changed
		WHERE target.run_id=changed.run_id AND target.owner_epoch<=$3`,
		targetID, changedRuns)
	if _, err := tx.Exec(ctx, deleteStages, after, last, epoch); err != nil {
		return 0, fmt.Errorf("readmodelpg: clear changed stages: %w", err)
	}
	stageRefresh := fmt.Sprintf(`
		INSERT INTO %[1]s.run_stage (`+stageCopyColumns+`)
		SELECT s.run_id,s.stage,s.run_terminal,s.had_success,s.had_failure,s.had_other,
		       s.token_measured,s.premium_measured,s.cost_measured,s.retry_waste,$3
		FROM %[2]s.run_stage AS s JOIN (%[3]s) AS changed USING (run_id)`,
		targetID, sourceID, changedRuns)
	if _, err := tx.Exec(ctx, stageRefresh, after, last, epoch); err != nil {
		return 0, fmt.Errorf("readmodelpg: refresh changed stages: %w", err)
	}
	watermarks := fmt.Sprintf(`
		INSERT INTO %[1]s.source_watermark (`+watermarkCopyColumns+`)
		SELECT w.run_id,w.source_seq,$3
		FROM %[2]s.source_watermark AS w JOIN (%[3]s) AS changed USING (run_id)
		ON CONFLICT (run_id) DO UPDATE
		SET source_seq=excluded.source_seq,owner_epoch=excluded.owner_epoch
		WHERE %[1]s.source_watermark.owner_epoch<=excluded.owner_epoch`,
		targetID, sourceID, changedRuns)
	if _, err := tx.Exec(ctx, watermarks, after, last, epoch); err != nil {
		return 0, fmt.Errorf("readmodelpg: refresh changed watermarks: %w", err)
	}
	progress := fmt.Sprintf(`
		UPDATE %s.meta
		SET catchup_seq=$1,next_change_seq=$1,owner_epoch=$2
		WHERE singleton AND owner_epoch<=$2`, targetID)
	tag, err := tx.Exec(ctx, progress, last, epoch)
	if err != nil {
		return 0, fmt.Errorf("readmodelpg: advance change copy: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return 0, ErrStaleOwner
	}
	return last, nil
}

// CreateEmptyVersion creates a completed inactive version for isolated
// conformance and recovery validation. Pointer locking makes it impossible to
// drop a version that became active while this call was waiting.
func (b *Builder) CreateEmptyVersion(ctx context.Context, version int) error {
	target, err := versionSchema(version)
	if err != nil {
		return err
	}
	return b.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		activeVersion, err := lockPointer(ctx, tx, epoch)
		if err != nil {
			return err
		}
		if activeVersion == version {
			return fmt.Errorf("readmodelpg: refusing to replace active schema %s", target)
		}
		return resetVersion(ctx, tx, target, 0, 0, true, epoch)
	})
}

func lockPointer(ctx context.Context, tx pgx.Tx, epoch int64) (int, error) {
	var activeVersion int
	if err := tx.QueryRow(ctx, `
		SELECT active_version FROM control.readmodel_version
		WHERE name='readmodel' AND owner_epoch<=$1
		FOR UPDATE`, epoch).Scan(&activeVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrStaleOwner
		}
		return 0, fmt.Errorf("readmodelpg: lock active version: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE control.readmodel_version SET owner_epoch=$1
		WHERE name='readmodel' AND owner_epoch<=$1`, epoch)
	if err != nil {
		return 0, fmt.Errorf("readmodelpg: claim model pointer: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return 0, ErrStaleOwner
	}
	return activeVersion, nil
}

func lockSourceProgress(
	ctx context.Context,
	tx pgx.Tx,
	source string,
) (next int64, floor int64, err error) {
	query := fmt.Sprintf(`
		SELECT next_change_seq,min_change_seq FROM %s.meta
		WHERE singleton FOR UPDATE`, pgx.Identifier{source}.Sanitize())
	if err := tx.QueryRow(ctx, query).Scan(&next, &floor); err != nil {
		return 0, 0, fmt.Errorf("readmodelpg: lock source progress: %w", err)
	}
	return next, floor, nil
}

func schemaMetaExists(ctx context.Context, tx pgx.Tx, schema string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".meta").Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("readmodelpg: inspect target schema: %w", err)
	}
	return exists, nil
}

func resetVersion(
	ctx context.Context,
	tx pgx.Tx,
	target string,
	sourceVersion int,
	sourceSeq int64,
	ready bool,
	epoch int64,
) error {
	targetID := pgx.Identifier{target}.Sanitize()
	if _, err := tx.Exec(ctx, `DROP SCHEMA IF EXISTS `+targetID+` CASCADE`); err != nil {
		return fmt.Errorf("readmodelpg: reset rebuild target: %w", err)
	}
	if _, err := tx.Exec(ctx, versionDDL(targetID)); err != nil {
		return fmt.Errorf("readmodelpg: create rebuild target: %w", err)
	}
	var source any
	if sourceVersion > 0 {
		source = sourceVersion
	}
	insert := fmt.Sprintf(`
		INSERT INTO %s.meta (
			epoch,min_change_seq,next_change_seq,ready,source_version,
			run_cursor,base_complete,catchup_seq,owner_epoch
		) VALUES ($1,$2,$3,$4,$5,'',$4,$3,$6)`, targetID)
	floor := sourceSeq
	if !ready {
		floor = sourceSeq + 1
	}
	if _, err := tx.Exec(ctx, insert,
		newEpoch(), floor, sourceSeq, ready, source, epoch); err != nil {
		return fmt.Errorf("readmodelpg: initialize rebuild target: %w", err)
	}
	return nil
}

func truncateVersion(ctx context.Context, tx pgx.Tx, target string) error {
	targetID := pgx.Identifier{target}.Sanitize()
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`TRUNCATE %s.run_stage,%s.change,%s.source_watermark,%s.run`,
		targetID, targetID, targetID, targetID)); err != nil {
		return fmt.Errorf("readmodelpg: restart rebuild tables: %w", err)
	}
	return nil
}

func versionDDL(schema string) string {
	return fmt.Sprintf(`
		CREATE SCHEMA %[1]s;
		CREATE TABLE %[1]s.meta (
			singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
			epoch text NOT NULL,
			min_change_seq bigint NOT NULL DEFAULT 0 CHECK (min_change_seq >= 0),
			next_change_seq bigint NOT NULL DEFAULT 0 CHECK (next_change_seq >= 0),
			built_at timestamptz NOT NULL DEFAULT clock_timestamp(),
			ready boolean NOT NULL DEFAULT false,
			source_version integer,
			run_cursor text NOT NULL DEFAULT '',
			base_complete boolean NOT NULL DEFAULT false,
			catchup_seq bigint NOT NULL DEFAULT 0 CHECK (catchup_seq >= 0),
			last_pass_at timestamptz,
			open_failures integer NOT NULL DEFAULT 0 CHECK (open_failures >= 0),
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
		);
		CREATE TABLE %[1]s.run (
			run_id text PRIMARY KEY,
			source_seq bigint NOT NULL CHECK (source_seq >= 0),
			projection jsonb NOT NULL,gaggle text NOT NULL,workflow text NOT NULL,
			phase text NOT NULL,terminal boolean NOT NULL,started_at timestamptz NOT NULL,
			last_activity_at timestamptz NOT NULL,disposition text NOT NULL,
			stages text[] NOT NULL DEFAULT '{}',any_token_measured boolean NOT NULL DEFAULT false,
			any_premium_measured boolean NOT NULL DEFAULT false,
			any_cost_measured boolean NOT NULL DEFAULT false,
			any_retry_waste boolean NOT NULL DEFAULT false,
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
		);
		CREATE TABLE %[1]s.run_stage (
			run_id text NOT NULL REFERENCES %[1]s.run(run_id) ON DELETE CASCADE,
			stage text NOT NULL,run_terminal boolean NOT NULL,had_success boolean NOT NULL,
			had_failure boolean NOT NULL,had_other boolean NOT NULL,
			token_measured boolean NOT NULL,premium_measured boolean NOT NULL,
			cost_measured boolean NOT NULL,retry_waste boolean NOT NULL,
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
			PRIMARY KEY(run_id,stage)
		);
		CREATE TABLE %[1]s.change (
			seq bigint PRIMARY KEY CHECK (seq > 0),changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
			kind text NOT NULL,run_id text NOT NULL,gaggle text NOT NULL,workflow text NOT NULL,
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
		);
		CREATE TABLE %[1]s.source_watermark (
			run_id text PRIMARY KEY,source_seq bigint NOT NULL CHECK (source_seq >= 0),
			owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
		);
		CREATE INDEX run_recency ON %[1]s.run(started_at DESC,run_id);
		CREATE INDEX run_activity ON %[1]s.run(last_activity_at DESC,run_id);
		CREATE INDEX run_gaggle_recency ON %[1]s.run(gaggle,started_at DESC,run_id);
		CREATE INDEX run_gaggle_workflow_recency ON %[1]s.run(gaggle,workflow,started_at DESC,run_id);
		CREATE INDEX run_phase_recency ON %[1]s.run(phase,started_at DESC,run_id);
		CREATE INDEX run_gaggle_phase_recency ON %[1]s.run(gaggle,phase,started_at DESC,run_id);
		CREATE INDEX run_gaggle_workflow_phase_recency
			ON %[1]s.run(gaggle,workflow,phase,started_at DESC,run_id);
		CREATE INDEX run_stages ON %[1]s.run USING gin(stages);
		CREATE INDEX stage_filter ON %[1]s.run_stage(stage,run_id);
	`, schema)
}

// Flip atomically exposes a complete version only if the source did not advance
// after its final catch-up batch.
func (b *Builder) Flip(ctx context.Context, name string, version int) error {
	if name != "readmodel" {
		return fmt.Errorf("readmodelpg: unknown model %q", name)
	}
	target, err := versionSchema(version)
	if err != nil {
		return err
	}
	return b.fence.WithFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		current, err := lockPointer(ctx, tx, epoch)
		if err != nil {
			return err
		}
		if current == version {
			return nil
		}
		if current > version {
			return fmt.Errorf("readmodelpg: active version %d is newer than %d", current, version)
		}
		var ready bool
		var targetSeq int64
		var sourceVersion *int
		query := fmt.Sprintf(`
			SELECT ready,next_change_seq,source_version FROM %s.meta
			WHERE singleton AND owner_epoch<=$1 FOR UPDATE`,
			pgx.Identifier{target}.Sanitize())
		if err := tx.QueryRow(ctx, query, epoch).Scan(
			&ready, &targetSeq, &sourceVersion,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleOwner
			}
			return fmt.Errorf("readmodelpg: validate rebuild target: %w", err)
		}
		if !ready || sourceVersion == nil || *sourceVersion != current {
			return fmt.Errorf("readmodelpg: rebuild target %s is incomplete", target)
		}
		source, err := versionSchema(current)
		if err != nil {
			return err
		}
		sourceSeq, _, err := lockSourceProgress(ctx, tx, source)
		if err != nil {
			return err
		}
		if sourceSeq != targetSeq {
			return fmt.Errorf(
				"readmodelpg: active model advanced during rebuild (%d to %d)",
				targetSeq, sourceSeq)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE control.readmodel_version
			SET active_version=$1,owner_epoch=$2,updated_at=clock_timestamp()
			WHERE name=$3 AND active_version=$4 AND owner_epoch<=$2`,
			version, epoch, name, current)
		if err != nil {
			return fmt.Errorf("readmodelpg: flip model pointer: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}
