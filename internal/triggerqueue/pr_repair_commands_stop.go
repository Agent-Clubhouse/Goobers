package triggerqueue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

// StopPRRepairCommand settles only intent that never won a write claim. It
// cannot interrupt, relabel or retry an attempting/unknown effect. This lets
// canceled turns release unused custody without pretending a provider ran.
func (s *Store) StopPRRepairCommand(ctx context.Context, scope WorkbenchCommandScope, id, digest string, now time.Time) (PRRepairCommand, error) {
	if !validWorkbenchScope(scope) || !validPRRepairID(id) || !validWorkbenchDigest(digest) || now.IsZero() {
		return PRRepairCommand{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PRRepairCommand{}, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := prRepairCommandTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	if record.RequestDigest != digest {
		return record, ErrConflict
	}
	if record.TombstonedAt != nil {
		return record, ErrWorkbenchCommandExpired
	}
	if record.State != "accepted" {
		return record, nil
	}
	if now.Before(record.AcceptedAt) {
		return record, ErrTransition
	}
	receipt := sessioning.PRRepairReceipt{OperationDigest: record.Input.OperationDigest, Outcome: "not-applied"}
	raw, _ := json.Marshal(receipt)
	res, err := tx.ExecContext(ctx, `UPDATE pr_repair_commands SET state='not-applied',receipt=?,receipt_digest=?,completed_ns=?,reserved_bytes=0 WHERE id=? AND state='accepted' AND attempted_ns IS NULL`, raw, childDigest(raw), now.UnixNano(), id)
	if err = changed(res, err); err != nil {
		return record, err
	}
	record, err = prRepairCommandTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	return record, tx.Commit()
}
