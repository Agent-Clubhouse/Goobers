package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

const sourceOccurrenceSchema = `ALTER TABLE source_start_cursors ADD COLUMN occurrence_cursors BLOB;`
const maxScheduleDeclarations = 1000

// SourceOccurrences initializes declaration positions from the adopted revision
// cursor once. A legacy pending fire must transfer before independent discovery.
func (s *Store) SourceOccurrences(ctx context.Context, scope, revision string, count int) ([]time.Time, bool, error) {
	if !validChildText(scope, 256, true) || !validChildText(revision, 128, true) || count < 1 || count > maxScheduleDeclarations {
		return nil, false, errors.New("triggerqueue: invalid occurrence cursor")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var ns int64
	var legacy bool
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT cursor_ns,legacy_pending,occurrence_cursors FROM source_start_cursors WHERE scope=? AND revision=?`, scope, revision).Scan(&ns, &legacy, &raw)
	if err != nil {
		return nil, false, err
	}
	if legacy {
		return nil, true, nil
	}
	var positions []int64
	if raw == nil {
		positions = make([]int64, count)
		for i := range positions {
			positions[i] = ns
		}
		raw, err = json.Marshal(positions)
		if err != nil {
			return nil, false, err
		}
		if err = childByteCapacity(ctx, tx, len(raw)+4096); err != nil {
			return nil, false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE source_start_cursors SET occurrence_cursors=? WHERE scope=? AND revision=?`, raw, scope, revision); err != nil {
			return nil, false, err
		}
	} else if positions, err = decodeOccurrencePositions(raw); err != nil {
		return nil, false, err
	}
	if len(positions) != count {
		return nil, false, ErrTransition
	}
	result := make([]time.Time, count)
	for i, position := range positions {
		result[i] = time.Unix(0, position).UTC()
	}
	return result, false, tx.Commit()
}

func decodeOccurrencePositions(raw []byte) ([]int64, error) {
	var positions []int64
	if len(raw) > 24*maxScheduleDeclarations {
		return nil, errors.New("triggerqueue: oversized occurrence cursor")
	}
	if err := json.Unmarshal(raw, &positions); err != nil {
		return nil, err
	}
	if len(positions) < 1 || len(positions) > maxScheduleDeclarations {
		return nil, errors.New("triggerqueue: invalid occurrence positions")
	}
	return positions, nil
}

func advanceSourceOccurrence(ctx context.Context, tx *sql.Tx, a *SourceAdvance) error {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT occurrence_cursors FROM source_start_cursors WHERE scope=? AND revision=? AND legacy_pending=0`, a.Scope, a.Revision).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTransition
		}
		return err
	}
	positions, err := decodeOccurrencePositions(raw)
	if err != nil {
		return err
	}
	index := a.Occurrence - 1
	if index < 0 || index >= len(positions) || positions[index] != a.Before.UnixNano() {
		return ErrTransition
	}
	positions[index] = a.After.UnixNano()
	updated, err := json.Marshal(positions)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE source_start_cursors SET occurrence_cursors=?,cursor_ns=? WHERE scope=? AND revision=? AND occurrence_cursors=?`, updated, slices.Min(positions), a.Scope, a.Revision, raw)
	return changed(result, err)
}
