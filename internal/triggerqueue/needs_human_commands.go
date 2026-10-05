package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/workbench"
)

// MaxNeedsHumanEvidenceBytes bounds source/evidence retained before an effect.
const MaxNeedsHumanEvidenceBytes = 1 << 20
const needsHumanReceiptAllowance = MaxWorkbenchReceiptBytes + 16*1024

// NeedsHumanCommandInput carries a host-verified observation and actual session
// origin. None of these fields authorize an effect without the current lease.
// Observation is required only for first acceptance; exact replay retains it.
type NeedsHumanCommandInput struct {
	Scope           WorkbenchCommandScope
	RequestID       string
	TargetDigest    string
	OperationDigest string
	Request         workbench.NeedsHumanResolutionRequest
	Origin          workbench.NeedsHumanResolutionOrigin
	Observation     *workbench.NeedsHumanObservation
}

// NeedsHumanCommand owns exactly one marker removal attempt. Unknown custody
// remains immutable and never frees capacity through age or later matching reads.
type NeedsHumanCommand struct {
	ID                                     string
	Input                                  NeedsHumanCommandInput
	RequestDigest                          string
	State                                  string
	AcceptedAt                             time.Time
	AttemptedAt, CompletedAt, TombstonedAt *time.Time
	Receipt                                *workbench.NeedsHumanResolutionReceipt
	ReceiptDigest                          string
}

// FindNeedsHumanCommand checks a verified actor's exact command key without
// rereading a naturally changed source revision after a confirmed mutation.
func (s *Store) FindNeedsHumanCommand(ctx context.Context, scope WorkbenchCommandScope, key string) (NeedsHumanCommand, error) {
	if !validWorkbenchScope(scope) || !validChildText(key, 256, true) {
		return NeedsHumanCommand{}, ErrTransition
	}
	result, err := scanNeedsHumanCommand(s.db.QueryRowContext(ctx, "SELECT "+needsHumanColumns+" FROM needs_human_commands WHERE key_digest=?", workbenchCommandKey(scope, key)))
	if err == nil && result.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	return result, err
}

// NeedsHumanCommand retrieves exact actor/source custody after host authorization.
func (s *Store) NeedsHumanCommand(ctx context.Context, scope WorkbenchCommandScope, id string) (NeedsHumanCommand, error) {
	if !validWorkbenchScope(scope) || !validNeedsHumanID(id) {
		return NeedsHumanCommand{}, ErrTransition
	}
	result, err := scanNeedsHumanCommand(s.db.QueryRowContext(ctx, "SELECT "+needsHumanColumns+" FROM needs_human_commands WHERE "+workbenchScopeWhere, workbenchScopeArgs(scope, id)...))
	if err == nil && result.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	return result, err
}

// AcceptNeedsHumanCommand commits bounded evidence and reserves receipt bytes
// before a marker attempt. Every command kind shares the same gaggle/quota cap.
func (s *Store) AcceptNeedsHumanCommand(ctx context.Context, input NeedsHumanCommandInput, now time.Time) (NeedsHumanCommand, bool, error) {
	request, digest, err := canonicalNeedsHumanRequest(input)
	if err != nil || now.IsZero() {
		return NeedsHumanCommand{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NeedsHumanCommand{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	key := workbenchCommandKey(input.Scope, input.RequestID)
	previous, err := scanNeedsHumanCommand(tx.QueryRowContext(ctx, "SELECT "+needsHumanColumns+" FROM needs_human_commands WHERE key_digest=?", key))
	if err == nil {
		return replayNeedsHumanCommand(previous, input, digest)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return NeedsHumanCommand{}, false, err
	}
	evidence, err := encodeNeedsHumanObservation(input)
	if err != nil {
		return NeedsHumanCommand{}, false, err
	}
	count, err := workbenchCommandCount(ctx, tx, input.Scope.Gaggle)
	if err != nil {
		return NeedsHumanCommand{}, false, err
	}
	if count >= MaxWorkbenchCommands {
		return NeedsHumanCommand{}, false, ErrFull
	}
	if err = childByteCapacity(ctx, tx, len(request)+len(evidence)+needsHumanReceiptAllowance+16*1024); err != nil {
		return NeedsHumanCommand{}, false, err
	}
	id := fmt.Sprintf("resolution-%x", randomID())
	_, err = tx.ExecContext(ctx, `INSERT INTO needs_human_commands(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,evidence,evidence_digest,state,accepted_ns,reserved_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'accepted',?,?)`, id, key, input.Scope.Gaggle, input.Scope.SourceBindingID, input.Scope.Actor.Issuer, input.Scope.Actor.Subject, input.RequestID, digest, input.TargetDigest, input.OperationDigest, request, evidence, childDigest(evidence), now.UnixNano(), needsHumanReceiptAllowance)
	if err != nil {
		return NeedsHumanCommand{}, false, err
	}
	result, err := needsHumanCommandTx(ctx, tx, input.Scope, id)
	if err != nil {
		return result, false, err
	}
	return result, false, tx.Commit()
}
func replayNeedsHumanCommand(previous NeedsHumanCommand, input NeedsHumanCommandInput, digest string) (NeedsHumanCommand, bool, error) {
	if previous.RequestDigest != digest || previous.Input.TargetDigest != input.TargetDigest || previous.Input.OperationDigest != input.OperationDigest {
		return NeedsHumanCommand{}, false, ErrConflict
	}
	if previous.TombstonedAt != nil {
		return previous, true, ErrWorkbenchCommandExpired
	}
	return previous, true, nil
}

// ClaimNeedsHumanCommand grants the single provider attempt atomically. Existing
// attempting/unknown custody is returned with claimed=false across restarts.
func (s *Store) ClaimNeedsHumanCommand(ctx context.Context, scope WorkbenchCommandScope, id, digest string, now time.Time) (NeedsHumanCommand, bool, error) {
	if !validWorkbenchScope(scope) || !validNeedsHumanID(id) || !validWorkbenchDigest(digest) || now.IsZero() {
		return NeedsHumanCommand{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NeedsHumanCommand{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := needsHumanCommandTx(ctx, tx, scope, id)
	if err != nil {
		return record, false, err
	}
	if record.RequestDigest != digest {
		return record, false, ErrConflict
	}
	if record.TombstonedAt != nil {
		return record, false, ErrWorkbenchCommandExpired
	}
	if record.State != "accepted" {
		return record, false, nil
	}
	if now.Before(record.AcceptedAt) {
		return record, false, ErrTransition
	}
	result, err := tx.ExecContext(ctx, `UPDATE needs_human_commands SET state='attempting',attempted_ns=? WHERE id=? AND state='accepted' AND request_digest=?`, now.UnixNano(), id, digest)
	if err != nil {
		return record, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return record, false, ErrTransition
	}
	stamp := now.UTC()
	record.State = "attempting"
	record.AttemptedAt = &stamp
	return record, true, tx.Commit()
}

// CompleteNeedsHumanCommand retains acknowledgement separately from observation.
// A later observation cannot promote a previously unknown effect to success.
func (s *Store) CompleteNeedsHumanCommand(ctx context.Context, scope WorkbenchCommandScope, id, digest string, receipt workbench.NeedsHumanResolutionReceipt, now time.Time) (NeedsHumanCommand, error) {
	if !validWorkbenchScope(scope) || !validNeedsHumanID(id) || !validWorkbenchDigest(digest) || now.IsZero() {
		return NeedsHumanCommand{}, ErrTransition
	}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > MaxWorkbenchReceiptBytes {
		return NeedsHumanCommand{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NeedsHumanCommand{}, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := needsHumanCommandTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	if record.RequestDigest != digest {
		return record, ErrConflict
	}
	if record.TombstonedAt != nil {
		return record, ErrWorkbenchCommandExpired
	}
	if err = validateNeedsHumanReceipt(record, receipt); err != nil {
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
	var reserved int
	if err = tx.QueryRowContext(ctx, `SELECT reserved_bytes FROM needs_human_commands WHERE id=?`, id).Scan(&reserved); err != nil {
		return record, err
	}
	if err = childByteCapacity(ctx, tx, len(raw)+8*1024-reserved); err != nil {
		return record, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE needs_human_commands SET state=?,receipt=?,receipt_digest=?,completed_ns=?,reserved_bytes=0 WHERE id=? AND state='attempting' AND receipt_digest=''`, receipt.Outcome, raw, receiptDigest, now.UnixNano(), id)
	if err != nil {
		return record, err
	}
	record, err = needsHumanCommandTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	return record, tx.Commit()
}
