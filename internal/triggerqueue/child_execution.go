package triggerqueue

import (
	"context"
	"errors"
)

// ActiveChildren is a bounded, cursor-based internal recovery scan. It carries
// each child's gaggle explicitly; it is not an authorization boundary or portal
// read API. A terminal row is removed only after observed durable result custody.
func (s *Store) ActiveChildren(ctx context.Context, after string, limit int) ([]ChildRecord, error) {
	if len(after) > 128 || limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid active child page")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+childColumns+childFrom+` WHERE c.child_id>? AND c.tombstoned_ns IS NULL AND c.state IN ('queued','running','awaiting_human') ORDER BY c.child_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []ChildRecord
	for rows.Next() {
		c, err := scanChild(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, c)
	}
	return records, rows.Err()
}

// ChildStart returns the original start receipt via its full lineage identity.
// Runtime recovery already owns this identity; external requests must use the
// authenticated SubmissionService instead of this internal read bridge.
func (s *Store) ChildStart(ctx context.Context, identity ChildIdentity) (Record, error) {
	if !identity.valid() {
		return Record{}, ErrTransition
	}
	return scanRecord(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM triggers WHERE id=(SELECT acceptance_id FROM child_lineages WHERE gaggle=? AND parent_run=? AND occurrence=? AND invocation_key=? AND tombstoned_ns IS NULL)`, childArgs(identity)...))
}
