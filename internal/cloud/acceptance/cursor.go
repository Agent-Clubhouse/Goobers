package acceptance

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// SourceCursorRevision initializes or observes one revisioned source cursor.
func (s *Store) SourceCursorRevision(
	ctx context.Context,
	scope, revision string,
	initial, observed time.Time,
	legacy bool,
) (time.Time, bool, error) {
	if !validText(scope, 256, true) || !validText(revision, 128, true) || initial.IsZero() || observed.IsZero() {
		return time.Time{}, false, errors.New("cloud acceptance: invalid source revision")
	}
	var cursor time.Time
	var pending bool
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, _ int64) error {
		var currentRevision string
		err := tx.QueryRow(txCtx, `
			SELECT revision, cursor_at, legacy_pending
			FROM control.source_cursor
			WHERE instance_id=$1 AND scope=$2
			FOR UPDATE`,
			s.instanceID, scope,
		).Scan(&currentRevision, &cursor, &pending)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(txCtx, `
				INSERT INTO control.source_cursor
					(instance_id, scope, revision, cursor_at, legacy_pending)
				VALUES ($1, $2, $3, $4, $5)`,
				s.instanceID, scope, revision, initial.UTC(), legacy,
			)
			cursor, pending = initial.UTC(), legacy
			return err
		}
		if err != nil {
			return err
		}
		if currentRevision != revision {
			if !observed.After(cursor) {
				return triggerqueue.ErrTransition
			}
			tag, err := tx.Exec(txCtx, `
				UPDATE control.source_cursor
				SET revision=$3, cursor_at=$4, legacy_pending=false, occurrence_cursors=NULL
				WHERE instance_id=$1 AND scope=$2 AND revision=$5 AND cursor_at<$4`,
				s.instanceID, scope, revision, observed.UTC(), currentRevision,
			)
			if err := changed(tag.RowsAffected(), err); err != nil {
				return err
			}
			cursor, pending = observed.UTC(), false
		}
		return nil
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return cursor.UTC(), pending, nil
}

// SourceOccurrences returns per-declaration cursor positions.
func (s *Store) SourceOccurrences(ctx context.Context, scope, revision string, count int) ([]time.Time, bool, error) {
	if !validText(scope, 256, true) || !validText(revision, 128, true) || count < 1 || count > 1000 {
		return nil, false, errors.New("cloud acceptance: invalid occurrence cursor")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var currentRevision string
	var cursor time.Time
	var pending bool
	var positions []time.Time
	err = tx.QueryRow(ctx, `
		SELECT revision, cursor_at, legacy_pending, occurrence_cursors
		FROM control.source_cursor
		WHERE instance_id=$1 AND scope=$2
		FOR UPDATE`,
		s.instanceID, scope,
	).Scan(&currentRevision, &cursor, &pending, &positions)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, sql.ErrNoRows
	}
	if err != nil {
		return nil, false, err
	}
	if currentRevision != revision {
		return nil, false, triggerqueue.ErrTransition
	}
	if pending {
		return nil, true, tx.Commit(ctx)
	}
	if positions == nil {
		positions = make([]time.Time, count)
		for i := range positions {
			positions[i] = cursor.UTC()
		}
		_, err = tx.Exec(ctx, `
			UPDATE control.source_cursor SET occurrence_cursors=$3
			WHERE instance_id=$1 AND scope=$2`,
			s.instanceID, scope, positions,
		)
	} else if len(positions) != count {
		return nil, false, triggerqueue.ErrTransition
	}
	if err != nil {
		return nil, false, err
	}
	return positions, false, tx.Commit(ctx)
}

// ResetExistingSourceRevision fences a removed schedule without creating one.
func (s *Store) ResetExistingSourceRevision(ctx context.Context, scope, revision string, observed time.Time) error {
	if !validText(scope, 256, true) || !validText(revision, 128, true) || observed.IsZero() {
		return errors.New("cloud acceptance: invalid source revision reset")
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, _ int64) error {
		_, err := tx.Exec(txCtx, `
			UPDATE control.source_cursor
			SET revision=$3, cursor_at=$4, legacy_pending=false, occurrence_cursors=NULL
			WHERE instance_id=$1 AND scope=$2 AND revision<>$3 AND cursor_at<$4`,
			s.instanceID, scope, revision, observed.UTC(),
		)
		return err
	})
}

func advanceCursor(ctx context.Context, tx pgx.Tx, instanceID string, advance *triggerqueue.SourceAdvance) error {
	if advance == nil {
		return nil
	}
	if !validText(advance.Scope, 256, true) || !validText(advance.Revision, 128, true) ||
		advance.Occurrence < 0 || advance.Before.IsZero() || !advance.After.After(advance.Before) {
		return triggerqueue.ErrTransition
	}
	if advance.Occurrence == 0 {
		tag, err := tx.Exec(ctx, `
			UPDATE control.source_cursor SET cursor_at=$5, legacy_pending=false
			WHERE instance_id=$1 AND scope=$2 AND revision=$3 AND cursor_at=$4
			  AND cursor_at<$5 AND occurrence_cursors IS NULL`,
			instanceID, advance.Scope, advance.Revision, advance.Before.UTC(), advance.After.UTC(),
		)
		return changed(tag.RowsAffected(), err)
	}
	var positions []time.Time
	err := tx.QueryRow(ctx, `
		SELECT occurrence_cursors FROM control.source_cursor
		WHERE instance_id=$1 AND scope=$2 AND revision=$3
		FOR UPDATE`,
		instanceID, advance.Scope, advance.Revision,
	).Scan(&positions)
	if err != nil || advance.Occurrence > len(positions) ||
		!positions[advance.Occurrence-1].Equal(advance.Before) {
		if err != nil {
			return err
		}
		return triggerqueue.ErrTransition
	}
	positions[advance.Occurrence-1] = advance.After.UTC()
	minimum := positions[0]
	for _, position := range positions[1:] {
		if position.Before(minimum) {
			minimum = position
		}
	}
	_, err = tx.Exec(ctx, `
		UPDATE control.source_cursor
		SET occurrence_cursors=$4, cursor_at=$5
		WHERE instance_id=$1 AND scope=$2 AND revision=$3`,
		instanceID, advance.Scope, advance.Revision, positions, minimum,
	)
	return err
}

var _ triggerqueue.ScheduleCursorStore = (*Store)(nil)
