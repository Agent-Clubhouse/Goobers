package triggerqueue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
)

const childDispositionSchema = `
ALTER TABLE child_lineages ADD COLUMN disposition_digest TEXT NOT NULL DEFAULT '';
CREATE TABLE child_dispositions (
 child_id TEXT PRIMARY KEY NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('merge','replace','discard')),
 result_ref TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 requested_ns INTEGER NOT NULL,
 plan BLOB NOT NULL CHECK(length(plan)<=131072),
 plan_digest TEXT NOT NULL,
 applied_ns INTEGER
);
CREATE TRIGGER child_release_disposition AFTER UPDATE OF tombstoned_ns ON child_lineages
WHEN OLD.tombstoned_ns IS NULL AND NEW.tombstoned_ns IS NOT NULL
BEGIN DELETE FROM child_dispositions WHERE child_id=OLD.child_id; END;
`

// ErrChildDispositionPending means no parent choice has been durably accepted.
var ErrChildDispositionPending = errors.New("triggerqueue: child disposition not requested")

// ChildDisposition is bounded, family-retained custody of one parent choice.
// Plan is a trusted host's exact application intent, persisted before effects.
type ChildDisposition struct {
	Identity    ChildIdentity
	Action      string
	ResultRef   string
	AttemptID   string
	RequestedAt time.Time
	Plan        []byte
	PlanDigest  string
	AppliedAt   time.Time
}

// ChildDispositionRequest contains only the parent's choice and expected result.
// Authority must already have been resolved from the signed current invocation.
type ChildDispositionRequest struct {
	Identity  ChildIdentity
	Action    string
	ResultRef string
	Authority ChildAuthority
}

func validDispositionAction(action string) bool {
	return action == "merge" || action == "replace" || action == "discard"
}

// RequestChildDisposition atomically fences stale attempts, parent cancellation
// and changed terminal results. Identical requests are idempotent. A requested
// disposition holds the occurrence slot until verified application is recorded.
func (s *Store) RequestChildDisposition(ctx context.Context, request ChildDispositionRequest, now time.Time) (ChildDisposition, error) {
	if !request.Identity.valid() || !validDispositionAction(request.Action) || !blobstore.ValidDigest(request.ResultRef) || request.Authority.ChildParent != request.Identity.ChildParent || request.Authority.StageOccurrence != request.Identity.StageOccurrence {
		return ChildDisposition{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChildDisposition{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkChildAuthority(ctx, tx, request.Authority, now); err != nil {
		return ChildDisposition{}, err
	}
	if err := childParentOpen(ctx, tx, request.Identity.ChildParent); err != nil {
		return ChildDisposition{}, err
	}
	child, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(request.Identity)...))
	if err != nil {
		return ChildDisposition{}, err
	}
	if !child.TombstonedAt.IsZero() || !child.State.Terminal() || child.ResultRef != request.ResultRef {
		return ChildDisposition{}, ErrTransition
	}
	prior, err := readChildDisposition(ctx, tx, child)
	if err == nil {
		if prior.Action != request.Action || prior.ResultRef != request.ResultRef {
			return ChildDisposition{}, ErrConflict
		}
		return prior, tx.Commit()
	}
	if !errors.Is(err, ErrChildDispositionPending) {
		return ChildDisposition{}, err
	}
	if !child.AcknowledgedAt.IsZero() || now.Before(child.UpdatedAt) {
		return ChildDisposition{}, ErrTransition
	}
	if err := consumeChildStorage(ctx, tx, child.ChildID, childRequestAllowance, 1024); err != nil {
		return ChildDisposition{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO child_dispositions(child_id,action,result_ref,attempt_id,requested_ns,plan,plan_digest) VALUES(?,?,?,?,?,x'','')`, child.ChildID, request.Action, request.ResultRef, request.Authority.AttemptID, now.UnixNano()); err != nil {
		return ChildDisposition{}, err
	}
	digest := dispositionRequestDigest(ChildDisposition{Identity: request.Identity, Action: request.Action, ResultRef: request.ResultRef, AttemptID: request.Authority.AttemptID, RequestedAt: now})
	if _, err := tx.ExecContext(ctx, `UPDATE child_lineages SET disposition_digest=? WHERE child_id=?`, digest, child.ChildID); err != nil {
		return ChildDisposition{}, err
	}
	result, err := readChildDisposition(ctx, tx, child)
	if err != nil {
		return ChildDisposition{}, err
	}
	return result, tx.Commit()
}

func readChildDisposition(ctx context.Context, reader childProposalReader, child ChildRecord) (ChildDisposition, error) {
	result := ChildDisposition{Identity: child.Identity}
	var marker string
	if err := reader.QueryRowContext(ctx, `SELECT disposition_digest FROM child_lineages WHERE child_id=? AND tombstoned_ns IS NULL`, child.ChildID).Scan(&marker); err != nil {
		return result, ErrConflict
	}
	if marker == "" {
		return result, ErrChildDispositionPending
	}
	var requested int64
	var applied sql.NullInt64
	err := reader.QueryRowContext(ctx, `SELECT action,result_ref,attempt_id,requested_ns,plan,plan_digest,applied_ns FROM child_dispositions WHERE child_id=? AND length(plan)<=131072`, child.ChildID).Scan(&result.Action, &result.ResultRef, &result.AttemptID, &requested, &result.Plan, &result.PlanDigest, &applied)
	if errors.Is(err, sql.ErrNoRows) {
		return ChildDisposition{}, ErrConflict
	}
	if err != nil {
		return ChildDisposition{}, err
	}
	result.RequestedAt = time.Unix(0, requested).UTC()
	if dispositionRequestDigest(result) != marker {
		return ChildDisposition{}, ErrConflict
	}
	if applied.Valid {
		result.AppliedAt = time.Unix(0, applied.Int64).UTC()
	}
	if !validDispositionAction(result.Action) || result.ResultRef != child.ResultRef || (len(result.Plan) == 0 && result.PlanDigest != "") || (len(result.Plan) != 0 && result.PlanDigest != "sha256:"+childDigest(result.Plan)) {
		return ChildDisposition{}, ErrConflict
	}
	return result, nil
}

func dispositionRequestDigest(d ChildDisposition) string {
	data, _ := json.Marshal(struct {
		Identity                     ChildIdentity
		Action, ResultRef, AttemptID string
		Requested                    int64
	}{d.Identity, d.Action, d.ResultRef, d.AttemptID, d.RequestedAt.UnixNano()})
	return "sha256:" + childDigest(data)
}

// ChildDisposition reads this qualified child's durable parent choice.
func (s *Store) ChildDisposition(ctx context.Context, identity ChildIdentity) (ChildDisposition, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil {
		return ChildDisposition{}, err
	}
	if !child.TombstonedAt.IsZero() {
		return ChildDisposition{}, ErrTransition
	}
	return readChildDisposition(ctx, s.db, child)
}

// KeepChildDispositionPlan precedes every filesystem change. There is at most
// one 128KiB plan per retained child, charged to the existing queue byte budget.
func (s *Store) KeepChildDispositionPlan(ctx context.Context, expected ChildDisposition, plan []byte) error {
	if len(plan) == 0 || len(plan) > MaxChildSnapshotReceiptBytes {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	child, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(expected.Identity)...))
	if err != nil {
		return err
	}
	if child.CancellationRequested || !child.TombstonedAt.IsZero() {
		return ErrParentCancelled
	}
	current, err := readChildDisposition(ctx, tx, child)
	if err != nil {
		return err
	}
	if !sameDispositionRequest(current, expected) {
		return ErrConflict
	}
	if len(current.Plan) != 0 {
		if !bytes.Equal(current.Plan, plan) {
			return ErrConflict
		}
		return tx.Commit()
	}
	if !current.AppliedAt.IsZero() {
		return ErrTransition
	}
	if err := consumeChildStorage(ctx, tx, child.ChildID, childPlanAllowance, len(plan)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE child_dispositions SET plan=?,plan_digest=? WHERE child_id=?`, plan, "sha256:"+childDigest(plan), child.ChildID); err != nil {
		return err
	}
	return tx.Commit()
}

func sameDispositionRequest(a, b ChildDisposition) bool {
	return a.Identity == b.Identity && a.Action == b.Action && a.ResultRef == b.ResultRef && a.AttemptID == b.AttemptID && a.RequestedAt.Equal(b.RequestedAt)
}

// CompleteChildDisposition records an already verified external effect and
// releases the occurrence atomically. Cancellation cannot erase that evidence.
// The host must VerifyChildApplication immediately before calling this method.
func (s *Store) CompleteChildDisposition(ctx context.Context, expected ChildDisposition, now time.Time) error {
	if !blobstore.ValidDigest(expected.PlanDigest) || now.IsZero() {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	child, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(expected.Identity)...))
	if err != nil {
		return err
	}
	current, err := readChildDisposition(ctx, tx, child)
	if err != nil {
		return err
	}
	if !sameDispositionRequest(current, expected) || current.PlanDigest != expected.PlanDigest || !child.TombstonedAt.IsZero() || !child.State.Terminal() {
		return ErrConflict
	}
	if !current.AppliedAt.IsZero() {
		return tx.Commit()
	}
	if now.Before(child.UpdatedAt) || now.Before(current.RequestedAt) {
		return ErrTransition
	}
	if _, err := tx.ExecContext(ctx, `UPDATE child_dispositions SET applied_ns=? WHERE child_id=?`, now.UnixNano(), child.ChildID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE child_lineages SET acknowledged_ns=?,updated_ns=? WHERE child_id=?`, now.UnixNano(), now.UnixNano(), child.ChildID); err != nil {
		return err
	}
	return tx.Commit()
}
