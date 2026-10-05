package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/providers"
)

// MaxPRRepairIntentBytes is an additional bound on encoded intent. Native
// file limits still apply. All command kinds share the same queue quota.
const MaxPRRepairIntentBytes = 2 << 20

// MaxPRRepairEvidenceBytes bounds retained native PR metadata.
const MaxPRRepairEvidenceBytes = 128 << 10
const prRepairReceiptAllowance = 16 << 10

// PRRepairCommandInput is trusted exact target/turn custody. Target is required
// only for first acceptance; receipts/replays do not reread a moved source head.
type PRRepairCommandInput struct {
	Scope                                    WorkbenchCommandScope
	RequestID, TargetDigest, OperationDigest string
	Request                                  sessioning.PRRepairRequest
	Origin                                   sessioning.PRRepairOrigin
	Selection                                sessioning.PRRepairTarget
	Target                                   *providers.RepairPullRequest
}

// PRRepairCommand retains immutable one-attempt effects and human attribution.
type PRRepairCommand struct {
	ID                                     string
	Input                                  PRRepairCommandInput
	RequestDigest, State                   string
	AcceptedAt                             time.Time
	AttemptedAt, CompletedAt, TombstonedAt *time.Time
	Receipt                                *sessioning.PRRepairReceipt
	ReceiptDigest                          string
	Observations                           []sessioning.PRRepairObservation
	OmittedObservations                    int64
}

// FindPRRepairCommand checks a verified actor's exact command key without
// rereading a naturally changed source head after a confirmed mutation.
func (s *Store) FindPRRepairCommand(ctx context.Context, scope WorkbenchCommandScope, key string) (PRRepairCommand, error) {
	if !validWorkbenchScope(scope) || !validChildText(key, 256, true) {
		return PRRepairCommand{}, ErrTransition
	}
	result, err := scanPRRepairCommand(s.db.QueryRowContext(ctx, "SELECT "+prRepairColumns+" FROM pr_repair_commands WHERE key_digest=?", workbenchCommandKey(scope, key)))
	if err == nil && result.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	return result, err
}

// PRRepairCommand retrieves exact actor/source custody after host authorization.
func (s *Store) PRRepairCommand(ctx context.Context, scope WorkbenchCommandScope, id string) (PRRepairCommand, error) {
	if !validWorkbenchScope(scope) || !validPRRepairID(id) {
		return PRRepairCommand{}, ErrTransition
	}
	result, err := scanPRRepairCommand(s.db.QueryRowContext(ctx, "SELECT "+prRepairColumns+" FROM pr_repair_commands WHERE "+workbenchScopeWhere, workbenchScopeArgs(scope, id)...))
	if err == nil && result.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	return result, err
}

// AcceptPRRepairCommand commits bounded evidence and reserves receipt bytes
// before a branch update attempt. Every command kind shares the same gaggle/quota cap.
func (s *Store) AcceptPRRepairCommand(ctx context.Context, input PRRepairCommandInput, now time.Time) (PRRepairCommand, bool, error) {
	request, digest, err := canonicalPRRepairRequest(input)
	if err != nil || now.IsZero() {
		return PRRepairCommand{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PRRepairCommand{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	key := workbenchCommandKey(input.Scope, input.RequestID)
	previous, err := scanPRRepairCommand(tx.QueryRowContext(ctx, "SELECT "+prRepairColumns+" FROM pr_repair_commands WHERE key_digest=?", key))
	if err == nil {
		return replayPRRepairCommand(previous, input, digest)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PRRepairCommand{}, false, err
	}
	if err := verifyPRRepairTurn(ctx, tx, input); err != nil {
		return PRRepairCommand{}, false, err
	}
	if err := verifyPRRepairParent(ctx, tx, input); err != nil {
		return PRRepairCommand{}, false, err
	}
	if err := verifyPRRepairWriterCustody(ctx, tx, input.Selection); err != nil {
		return PRRepairCommand{}, false, err
	}
	evidence, err := encodePRRepairTarget(input)
	if err != nil {
		return PRRepairCommand{}, false, err
	}
	count, err := workbenchCommandCount(ctx, tx, input.Scope.Gaggle)
	if err != nil {
		return PRRepairCommand{}, false, err
	}
	if count >= MaxWorkbenchCommands {
		return PRRepairCommand{}, false, ErrFull
	}
	if err = childByteCapacity(ctx, tx, len(request)+len(evidence)+prRepairReceiptAllowance+16*1024); err != nil {
		return PRRepairCommand{}, false, err
	}
	id := fmt.Sprintf("repair-%x", randomID())
	_, err = tx.ExecContext(ctx, `INSERT INTO pr_repair_commands(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,evidence,evidence_digest,state,accepted_ns,reserved_bytes,turn_id,parent_id,physical_target) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'accepted',?,?,?,?,?)`, id, key, input.Scope.Gaggle, input.Scope.SourceBindingID, input.Scope.Actor.Issuer, input.Scope.Actor.Subject, input.RequestID, digest, input.TargetDigest, input.OperationDigest, request, evidence, childDigest(evidence), now.UnixNano(), prRepairReceiptAllowance, input.Origin.TurnID, repairNullable(input.Request.ParentCommandID), prRepairPhysicalTarget(input.Selection))
	if err != nil {
		return PRRepairCommand{}, false, err
	}
	result, err := prRepairCommandTx(ctx, tx, input.Scope, id)
	if err != nil {
		return result, false, err
	}
	return result, false, tx.Commit()
}
func replayPRRepairCommand(previous PRRepairCommand, input PRRepairCommandInput, digest string) (PRRepairCommand, bool, error) {
	if previous.RequestDigest != digest || previous.Input.TargetDigest != input.TargetDigest || previous.Input.OperationDigest != input.OperationDigest {
		return PRRepairCommand{}, false, ErrConflict
	}
	if previous.TombstonedAt != nil {
		return previous, true, ErrWorkbenchCommandExpired
	}
	return previous, true, nil
}

// ClaimPRRepairCommand grants the single provider attempt atomically. Existing
// attempting/unknown custody is returned with claimed=false across restarts.
func (s *Store) ClaimPRRepairCommand(ctx context.Context, scope WorkbenchCommandScope, id, digest string, now time.Time) (PRRepairCommand, bool, error) {
	if !validWorkbenchScope(scope) || !validPRRepairID(id) || !validWorkbenchDigest(digest) || now.IsZero() {
		return PRRepairCommand{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PRRepairCommand{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := prRepairCommandTx(ctx, tx, scope, id)
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
	if err = verifyPRRepairTurn(ctx, tx, record.Input); err != nil {
		return record, false, err
	}
	if now.Before(record.AcceptedAt) {
		return record, false, ErrTransition
	}
	result, err := tx.ExecContext(ctx, `UPDATE pr_repair_commands SET state='attempting',attempted_ns=? WHERE id=? AND state='accepted' AND request_digest=?`, now.UnixNano(), id, digest)
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

// CompletePRRepairCommand retains acknowledgement separately from observation.
// A later observation cannot promote a previously unknown effect to success.
func (s *Store) CompletePRRepairCommand(ctx context.Context, scope WorkbenchCommandScope, id, digest string, receipt sessioning.PRRepairReceipt, now time.Time) (PRRepairCommand, error) {
	if !validWorkbenchScope(scope) || !validPRRepairID(id) || !validWorkbenchDigest(digest) || now.IsZero() {
		return PRRepairCommand{}, ErrTransition
	}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > prRepairReceiptAllowance {
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
	if err = validatePRRepairReceipt(record, receipt); err != nil {
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
	if err = tx.QueryRowContext(ctx, `SELECT reserved_bytes FROM pr_repair_commands WHERE id=?`, id).Scan(&reserved); err != nil {
		return record, err
	}
	if err = childByteCapacity(ctx, tx, len(raw)+8*1024-reserved); err != nil {
		return record, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE pr_repair_commands SET state=?,receipt=?,receipt_digest=?,completed_ns=?,reserved_bytes=0 WHERE id=? AND state='attempting' AND receipt_digest=''`, receipt.Outcome, raw, receiptDigest, now.UnixNano(), id)
	if err != nil {
		return record, err
	}
	record, err = prRepairCommandTx(ctx, tx, scope, id)
	if err != nil {
		return record, err
	}
	return record, tx.Commit()
}
