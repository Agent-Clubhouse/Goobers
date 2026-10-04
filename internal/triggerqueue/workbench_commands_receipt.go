package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/goobers/goobers/internal/workbench"
)

// CompleteWorkbenchCommand retains the one host-observed attempt result. It is
// replay-safe for identical receipts only, including unknown outcomes. An unknown
// effect never becomes confirmed merely because a later read happens to match.
func (s *Store) CompleteWorkbenchCommand(ctx context.Context, scope WorkbenchCommandScope, id, digest string, receipt workbench.BacklogPatchReceipt, now time.Time) (WorkbenchCommand, error) {
	if !validWorkbenchScope(scope) || !validWorkbenchID(id) || !validWorkbenchDigest(digest) || now.IsZero() {
		return WorkbenchCommand{}, ErrTransition
	}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > MaxWorkbenchReceiptBytes {
		return WorkbenchCommand{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkbenchCommand{}, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := workbenchCommandTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	if record.RequestDigest != digest {
		return record, ErrConflict
	}
	if record.TombstonedAt != nil {
		return record, ErrWorkbenchCommandExpired
	}
	if err = validateWorkbenchReceipt(record, receipt); err != nil {
		return record, err
	}
	receiptDigest := childDigest(raw)
	if record.Receipt != nil {
		if record.ReceiptDigest != receiptDigest {
			return record, ErrConflict
		}
		return record, nil
	}
	if record.State != "attempting" || record.AttemptedAt == nil || now.Before(*record.AttemptedAt) {
		return record, ErrTransition
	}
	if err = completeWorkbenchStorage(ctx, tx, id, len(raw)); err != nil {
		return record, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE workbench_commands SET state=?,receipt=?,receipt_digest=?,completed_ns=?,reserved_bytes=0 WHERE id=? AND state='attempting' AND receipt_digest=''`, receipt.Outcome, raw, receiptDigest, now.UnixNano(), id)
	if err != nil {
		return record, err
	}
	record, err = workbenchCommandTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	return record, tx.Commit()
}
func completeWorkbenchStorage(ctx context.Context, tx *sql.Tx, id string, size int) error {
	var reserved int
	if err := tx.QueryRowContext(ctx, `SELECT reserved_bytes FROM workbench_commands WHERE id=?`, id).Scan(&reserved); err != nil {
		return err
	}
	return childByteCapacity(ctx, tx, size+8*1024-reserved)
}
func validateWorkbenchReceipt(record WorkbenchCommand, receipt workbench.BacklogPatchReceipt) error {
	if receipt.OperationDigest != record.Input.OperationDigest || (receipt.RevisionSemantics != "timestamp-preflight" && receipt.RevisionSemantics != "atomic-revision-test") {
		return ErrConflict
	}
	switch receipt.Outcome {
	case "confirmed":
		if !receipt.ProviderAcknowledged || !receipt.ObservedMatches || receipt.Observed == nil {
			return ErrTransition
		}
	case "not-applied":
		if receipt.ProviderAcknowledged || receipt.ObservedMatches {
			return ErrTransition
		}
	case "unknown":
	default:
		return ErrTransition
	}
	if receipt.ObservedMatches && receipt.Observed == nil {
		return ErrTransition
	}
	if observed := receipt.Observed; observed != nil {
		input := record.Input
		if observed.Ref.GaggleID != input.Scope.Gaggle || observed.Ref.SourceBindingID != input.Scope.SourceBindingID || observed.Ref.Kind != "work-item" || observed.Ref.SourceID != input.Request.SourceID || observed.Locator.ID != input.Request.ID {
			return ErrConflict
		}
		raw, err := json.Marshal(observed)
		if err != nil || len(raw) > workbench.MaxBacklogItemBytes {
			return ErrTransition
		}
	}
	return nil
}
