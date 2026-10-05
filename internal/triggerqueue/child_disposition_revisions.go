package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// MaxChildDispositionRevisions bounds retained prior choices per accepted child.
const MaxChildDispositionRevisions = 32
const childDispositionHistoryAllowance = MaxChildDispositionRevisions * 4096

const childDispositionHistorySchema = `
ALTER TABLE child_dispositions ADD COLUMN previous_digest TEXT NOT NULL DEFAULT '';
CREATE TABLE child_disposition_history (
 child_id TEXT NOT NULL, revision INTEGER NOT NULL CHECK(revision BETWEEN 1 AND 32),
 receipt BLOB NOT NULL CHECK(length(receipt)<=4096),
 PRIMARY KEY(child_id,revision)
);
CREATE TRIGGER child_release_disposition_history AFTER DELETE ON child_dispositions
BEGIN DELETE FROM child_disposition_history WHERE child_id=OLD.child_id; END;
UPDATE child_lineages SET reserved_bytes=reserved_bytes+131072
WHERE proposal_digest!='' AND tombstoned_ns IS NULL AND acknowledged_ns IS NULL;
`

var (
	// ErrChildDispositionReconcile refuses replacing a published application.
	ErrChildDispositionReconcile = errors.New("triggerqueue: published child application plan requires reconciliation before a different action")
	// ErrChildDispositionHistoryFull refuses growth beyond retained audit custody.
	ErrChildDispositionHistoryFull = errors.New("triggerqueue: child disposition revision history is full")
)

// ChildDispositionRevision is the bounded audit record retained when a current
// owner supersedes a choice. Source text, paths and credentials are excluded.
type ChildDispositionRevision struct {
	RequestDigest  string    `json:"requestDigest"`
	PreviousDigest string    `json:"previousDigest,omitempty"`
	Action         string    `json:"action"`
	ResultRef      string    `json:"resultRef"`
	AttemptID      string    `json:"attemptId"`
	RequestedAt    time.Time `json:"requestedAt"`
	PlanDigest     string    `json:"planDigest,omitempty"`
	SupersededAt   time.Time `json:"supersededAt"`
}

func (s *Store) reviseChildDisposition(ctx context.Context, tx *sql.Tx, child ChildRecord, prior ChildDisposition, request ChildDispositionRequest, now time.Time) (ChildDisposition, error) {
	sameChoice := prior.Action == request.Action && prior.ResultRef == request.ResultRef
	sameOwner := prior.AttemptID == request.Authority.AttemptID
	// A lost response may repeat exactly the request that created this revision.
	if sameChoice && (sameOwner || !prior.AppliedAt.IsZero()) && request.ExpectedRequestDigest == prior.PreviousDigest {
		return prior, tx.Commit()
	}
	if request.ExpectedRequestDigest != prior.RequestDigest() || request.ExpectedRequestDigest == "" {
		return ChildDisposition{}, ErrConflict
	}
	if sameChoice && sameOwner {
		return prior, tx.Commit()
	}
	if !prior.AppliedAt.IsZero() || !child.AcknowledgedAt.IsZero() {
		return ChildDisposition{}, ErrTransition
	}
	if len(prior.Plan) != 0 && !sameChoice {
		return ChildDisposition{}, ErrChildDispositionReconcile
	}
	if now.Before(prior.RequestedAt) || now.Before(child.UpdatedAt) {
		return ChildDisposition{}, ErrTransition
	}
	if err := s.archiveChildDisposition(ctx, tx, child, prior, now); err != nil {
		return ChildDisposition{}, err
	}
	// Rebinding an identical published plan to a replacement attempt retains the
	// exact application. A different action can only replace an unpublished plan.
	next := prior
	next.Action, next.AttemptID, next.RequestedAt, next.PreviousDigest = request.Action, request.Authority.AttemptID, now, prior.RequestDigest()
	if _, err := tx.ExecContext(ctx, `UPDATE child_dispositions SET action=?,attempt_id=?,requested_ns=?,previous_digest=? WHERE child_id=?`, next.Action, next.AttemptID, now.UnixNano(), next.PreviousDigest, child.ChildID); err != nil {
		return ChildDisposition{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE child_lineages SET disposition_digest=? WHERE child_id=?`, next.RequestDigest(), child.ChildID); err != nil {
		return ChildDisposition{}, err
	}
	return next, tx.Commit()
}

func (s *Store) archiveChildDisposition(ctx context.Context, tx *sql.Tx, child ChildRecord, prior ChildDisposition, now time.Time) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_disposition_history WHERE child_id=?`, child.ChildID).Scan(&count); err != nil {
		return err
	}
	if count >= MaxChildDispositionRevisions {
		return ErrChildDispositionHistoryFull
	}
	record := ChildDispositionRevision{RequestDigest: prior.RequestDigest(), PreviousDigest: prior.PreviousDigest, Action: prior.Action, ResultRef: prior.ResultRef, AttemptID: prior.AttemptID, RequestedAt: prior.RequestedAt, PlanDigest: prior.PlanDigest, SupersededAt: now}
	data, err := json.Marshal(record)
	if err != nil || len(data) > 4096 {
		return ErrTransition
	}
	if err := consumeChildStorage(ctx, tx, child.ChildID, 4096, len(data)+1024); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO child_disposition_history(child_id,revision,receipt) VALUES(?,?,?)`, child.ChildID, count+1, data)
	return err
}

// ChildDispositionHistory returns at most 32 immutable prior decisions. This
// host API requires caller ownership checks, like ChildDisposition itself.
func (s *Store) ChildDispositionHistory(ctx context.Context, identity ChildIdentity) ([]ChildDispositionRevision, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil {
		return nil, err
	}
	if !child.TombstonedAt.IsZero() {
		return nil, ErrTransition
	}
	rows, err := s.db.QueryContext(ctx, `SELECT receipt FROM child_disposition_history WHERE child_id=? ORDER BY revision LIMIT 32`, child.ChildID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var history []ChildDispositionRevision
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var record ChildDispositionRevision
		if len(data) > 4096 || json.Unmarshal(data, &record) != nil {
			return nil, ErrConflict
		}
		history = append(history, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	current, err := s.ChildDisposition(ctx, identity)
	if err != nil {
		return nil, err
	}
	previous := ""
	for _, record := range history {
		entry := ChildDisposition{Identity: identity, Action: record.Action, ResultRef: record.ResultRef, AttemptID: record.AttemptID, RequestedAt: record.RequestedAt, PreviousDigest: record.PreviousDigest}
		if record.RequestDigest != entry.RequestDigest() || record.PreviousDigest != previous || record.SupersededAt.Before(record.RequestedAt) {
			return nil, ErrConflict
		}
		previous = record.RequestDigest
	}
	if current.PreviousDigest != previous {
		return nil, ErrConflict
	}
	return history, nil
}
