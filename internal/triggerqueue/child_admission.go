package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ChildAdmissionRejectedReason is the durable start-rejection reason recorded
// when an operator rejects a held child before dispatch.
const ChildAdmissionRejectedReason = "child admission rejected by human review"

// ChildAdmissionState is the reviewable decision for a held child start.
type ChildAdmissionState string

// Child admission review states. Only approved releases a held start.
const (
	ChildAdmissionPending  ChildAdmissionState = "pending"
	ChildAdmissionApproved ChildAdmissionState = "approved"
	ChildAdmissionRejected ChildAdmissionState = "rejected"
)

// MaxChildAdmissionEvidenceBytes bounds the opaque evidence retained for review.
const MaxChildAdmissionEvidenceBytes = 4096

// ChildAdmissionHold asks AcceptChild to retain custody while fencing dispatch
// until an operator decides. Evidence is opaque, bounded, caller-validated JSON.
type ChildAdmissionHold struct {
	Reason   string
	Evidence []byte
}

// ChildAdmissionReview is the durable pending or decided admission hold.
type ChildAdmissionReview struct {
	Identity    ChildIdentity
	ChildID     string
	Reason      string
	Evidence    []byte
	State       ChildAdmissionState
	Actor       string
	Note        string
	RequestedAt time.Time
	DecidedAt   time.Time
}

// ChildAdmissionDecision approves or rejects one held child by its child ID.
type ChildAdmissionDecision struct {
	ChildID string
	Approve bool
	Actor   string
	Note    string
}

const childAdmissionSchema = `
CREATE TABLE child_admission_holds (
 acceptance_id TEXT PRIMARY KEY NOT NULL,
 reason TEXT NOT NULL CHECK(length(CAST(reason AS BLOB)) BETWEEN 1 AND 128),
 evidence BLOB NOT NULL CHECK(length(evidence) BETWEEN 1 AND 4096),
 state TEXT NOT NULL CHECK(state IN ('pending','approved','rejected')),
 actor TEXT NOT NULL DEFAULT '' CHECK(length(CAST(actor AS BLOB)) <= 1024),
 note TEXT NOT NULL DEFAULT '' CHECK(length(CAST(note AS BLOB)) <= 1024),
 requested_ns INTEGER NOT NULL, decided_ns INTEGER,
 CHECK((state='pending') = (decided_ns IS NULL))
);
CREATE INDEX child_admission_pending ON child_admission_holds(state,acceptance_id);
-- A held start keeps ordinary custody but cannot be claimed until approved.
CREATE TRIGGER child_admission_dispatch BEFORE UPDATE OF state ON triggers
WHEN NEW.state='dispatching' AND OLD.state='accepted' AND EXISTS(
 SELECT 1 FROM child_admission_holds h WHERE h.acceptance_id=OLD.id AND h.state<>'approved')
BEGIN SELECT RAISE(IGNORE); END;
CREATE TRIGGER child_admission_prune AFTER DELETE ON triggers
BEGIN DELETE FROM child_admission_holds WHERE acceptance_id=OLD.id; END;
`

func (h *ChildAdmissionHold) validate() error {
	if h == nil {
		return nil
	}
	if !validChildText(h.Reason, 128, true) || len(h.Evidence) == 0 || len(h.Evidence) > MaxChildAdmissionEvidenceBytes {
		return errors.New("triggerqueue: invalid child admission hold")
	}
	return nil
}

func insertChildAdmissionHold(ctx context.Context, tx *sql.Tx, acceptanceID string, hold *ChildAdmissionHold, now time.Time) error {
	if hold == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO child_admission_holds(acceptance_id,reason,evidence,state,requested_ns) VALUES(?,?,?,?,?)`, acceptanceID, hold.Reason, hold.Evidence, ChildAdmissionPending, now.UnixNano())
	return err
}

const childAdmissionColumns = `c.gaggle,c.parent_run,c.occurrence,c.invocation_key,c.child_id,h.reason,h.evidence,h.state,h.actor,h.note,h.requested_ns,h.decided_ns`
const childAdmissionFrom = ` FROM child_admission_holds h JOIN child_lineages c ON c.acceptance_id=h.acceptance_id`

func scanChildAdmission(row scanner) (ChildAdmissionReview, error) {
	var r ChildAdmissionReview
	var requested int64
	var decided sql.NullInt64
	if err := row.Scan(&r.Identity.Gaggle, &r.Identity.ParentRunID, &r.Identity.StageOccurrence, &r.Identity.InvocationKey,
		&r.ChildID, &r.Reason, &r.Evidence, &r.State, &r.Actor, &r.Note, &requested, &decided); err != nil {
		return ChildAdmissionReview{}, err
	}
	r.RequestedAt = time.Unix(0, requested).UTC()
	if decided.Valid {
		r.DecidedAt = time.Unix(0, decided.Int64).UTC()
	}
	return r, nil
}

// ChildAdmission returns the admission hold for an exact child identity, or
// sql.ErrNoRows when the child was admitted without review.
func (s *Store) ChildAdmission(ctx context.Context, identity ChildIdentity) (ChildAdmissionReview, error) {
	if !identity.valid() {
		return ChildAdmissionReview{}, errors.New("triggerqueue: invalid child identity")
	}
	return scanChildAdmission(s.db.QueryRowContext(ctx, "SELECT "+childAdmissionColumns+childAdmissionFrom+childWhere, childArgs(identity)...))
}

// PendingChildAdmissions pages undecided holds in child ID order after the
// exclusive cursor. Listing is operator-local and grants no approval authority.
func (s *Store) PendingChildAdmissions(ctx context.Context, after string, limit int) ([]ChildAdmissionReview, error) {
	if limit < 1 || limit > MaxRecords {
		return nil, errors.New("triggerqueue: invalid child admission page")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+childAdmissionColumns+childAdmissionFrom+` WHERE h.state='pending' AND c.child_id>? ORDER BY c.child_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ChildAdmissionReview
	for rows.Next() {
		r, err := scanChildAdmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DecideChildAdmission records one immutable operator decision. Repeating the
// same decision is idempotent; a conflicting later decision is ErrTransition.
// Rejection also rejects the still-accepted start so the parent observes a
// terminal child result instead of waiting forever.
func (s *Store) DecideChildAdmission(ctx context.Context, d ChildAdmissionDecision, now time.Time) (ChildAdmissionReview, error) {
	if !validChildText(d.ChildID, 256, true) || !validChildText(d.Actor, 1024, true) || !validChildText(d.Note, 1024, false) || now.IsZero() {
		return ChildAdmissionReview{}, errors.New("triggerqueue: invalid child admission decision")
	}
	state := ChildAdmissionRejected
	if d.Approve {
		state = ChildAdmissionApproved
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChildAdmissionReview{}, err
	}
	defer func() { _ = tx.Rollback() }()
	byID := "SELECT " + childAdmissionColumns + childAdmissionFrom + ` WHERE c.child_id=?`
	current, err := scanChildAdmission(tx.QueryRowContext(ctx, byID, d.ChildID))
	if err != nil {
		return ChildAdmissionReview{}, err
	}
	if current.State != ChildAdmissionPending {
		if current.State != state {
			return ChildAdmissionReview{}, ErrTransition
		}
		return current, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_admission_holds SET state=?,actor=?,note=?,decided_ns=? WHERE state='pending' AND acceptance_id=(SELECT acceptance_id FROM child_lineages WHERE child_id=?)`, state, d.Actor, d.Note, now.UnixNano(), d.ChildID); err != nil {
		return ChildAdmissionReview{}, err
	}
	if !d.Approve {
		if _, err = tx.ExecContext(ctx, `UPDATE triggers SET state='rejected',run_id='',reason=?,finished_ns=? WHERE state='accepted' AND id=(SELECT acceptance_id FROM child_lineages WHERE child_id=?)`, ChildAdmissionRejectedReason, now.UnixNano(), d.ChildID); err != nil {
			return ChildAdmissionReview{}, err
		}
	}
	decided, err := scanChildAdmission(tx.QueryRowContext(ctx, byID, d.ChildID))
	if err != nil {
		return ChildAdmissionReview{}, err
	}
	return decided, tx.Commit()
}
