package triggerqueue

import (
	"context"
	"errors"
	"time"
)

// PendingCursor identifies an immutable acceptance position. Hosts advance past
// held or unsupported starts, wrapping to the beginning after exhausting a pass.
// It is scheduling progress only; it never releases or changes durable custody.
type PendingCursor struct {
	AcceptedAt time.Time
	ID         string
}

// PendingAfter returns one FIFO window after a prior acceptance position. It
// excludes dispatch uncertainty and direct-engine work just like Pending. A
// deleted or dispatched cursor record still denotes the same ordering boundary.
func (s *Store) PendingAfter(ctx context.Context, after PendingCursor, limit int) ([]Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: batch limit must be 1..100")
	}
	if after.ID != "" && (len(after.ID) > 128 || after.AcceptedAt.IsZero()) {
		return nil, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM triggers WHERE state='accepted' AND NOT EXISTS(SELECT 1 FROM direct_engine_inputs i WHERE i.acceptance_id=triggers.id) AND (?='' OR accepted_ns>? OR (accepted_ns=? AND id>?)) ORDER BY accepted_ns,id LIMIT ?", after.ID, after.AcceptedAt.UnixNano(), after.AcceptedAt.UnixNano(), after.ID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
