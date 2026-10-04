package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

const (
	// MaxWorkbenchCommands includes full records and compact replay tombstones.
	MaxWorkbenchCommands = 1000
	// MaxWorkbenchRequestBytes caps canonical native mutation input.
	MaxWorkbenchRequestBytes = 256 << 10
	// MaxWorkbenchReceiptBytes includes one bounded source observation.
	MaxWorkbenchReceiptBytes = workbench.MaxBacklogItemBytes + 16*1024
	// WorkbenchCommandRetention applies to full evidence, then compact tombstones.
	WorkbenchCommandRetention = 30 * 24 * time.Hour
	workbenchReceiptAllowance = MaxWorkbenchReceiptBytes + 16*1024
)

// ErrWorkbenchCommandExpired reports a compact replay tombstone. A successful
// command has a 60-day idempotency window; unresolved custody never expires.
var ErrWorkbenchCommandExpired = errors.New("workbench command receipt expired")

// WorkbenchCommandScope is verified host authority, not caller-supplied JSON.
// Every caller must reauthorize this exact actor, gaggle and source target before
// accepting, inspecting, replaying or claiming command custody. A trusted host
// may still retain its already-claimed attempt receipt after policy cancellation.
type WorkbenchCommandScope struct {
	Gaggle          string
	SourceBindingID string
	Actor           sessioning.Actor
}

// WorkbenchCommandInput contains no credential, endpoint or execution run. The
// typed native request is canonicalized here. TargetDigest binds the host-selected
// target; OperationDigest is the provider adapter's exact source/request digest.
type WorkbenchCommandInput struct {
	Scope           WorkbenchCommandScope
	RequestID       string
	TargetDigest    string
	OperationDigest string
	Request         workbench.BacklogPatchRequest
}

// WorkbenchCommand records one external effect attempt and its immutable receipt.
// Attempting includes lost-response/process-death ambiguity; it cannot be claimed
// again. ObservedMatches alone is never evidence of provider acknowledgement.
type WorkbenchCommand struct {
	ID            string
	Input         WorkbenchCommandInput
	RequestDigest string
	State         string
	AcceptedAt    time.Time
	AttemptedAt   *time.Time
	CompletedAt   *time.Time
	TombstonedAt  *time.Time
	Receipt       *workbench.BacklogPatchReceipt
	ReceiptDigest string
}

// AcceptWorkbenchCommand reserves the worst-case receipt before any provider
// mutation. Exact repeats return original custody regardless of queue fullness.
// It does not authorize the actor or perform provider preflight validation.
func (s *Store) AcceptWorkbenchCommand(ctx context.Context, input WorkbenchCommandInput, now time.Time) (WorkbenchCommand, bool, error) {
	raw, digest, err := canonicalWorkbenchInput(input)
	if err != nil || now.IsZero() {
		return WorkbenchCommand{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkbenchCommand{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	key := workbenchCommandKey(input.Scope, input.RequestID)
	previous, err := scanWorkbenchCommand(tx.QueryRowContext(ctx, "SELECT "+workbenchCommandColumns+" FROM workbench_commands WHERE key_digest=?", key))
	if err == nil {
		return replayWorkbenchCommand(previous, digest, input.TargetDigest, input.OperationDigest)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkbenchCommand{}, false, err
	}
	count, err := workbenchCommandCount(ctx, tx, input.Scope.Gaggle)
	if err != nil {
		return WorkbenchCommand{}, false, err
	}
	if count >= MaxWorkbenchCommands {
		return WorkbenchCommand{}, false, ErrFull
	}
	if err = childByteCapacity(ctx, tx, len(raw)+workbenchReceiptAllowance+16*1024); err != nil {
		return WorkbenchCommand{}, false, err
	}
	id := fmt.Sprintf("workbench-%x", randomID())
	_, err = tx.ExecContext(ctx, `INSERT INTO workbench_commands(id,key_digest,gaggle,source_binding,issuer,subject,request_id,request_digest,target_digest,operation_digest,request,state,accepted_ns,reserved_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?,'accepted',?,?)`, id, key, input.Scope.Gaggle, input.Scope.SourceBindingID, input.Scope.Actor.Issuer, input.Scope.Actor.Subject, input.RequestID, digest, input.TargetDigest, input.OperationDigest, raw, now.UnixNano(), workbenchReceiptAllowance)
	if err != nil {
		return WorkbenchCommand{}, false, err
	}
	result, err := workbenchCommandTx(ctx, tx, input.Scope, id)
	if err != nil {
		return WorkbenchCommand{}, false, err
	}
	return result, false, tx.Commit()
}

func replayWorkbenchCommand(record WorkbenchCommand, digest, target, operation string) (WorkbenchCommand, bool, error) {
	if record.RequestDigest != digest || record.Input.TargetDigest != target || record.Input.OperationDigest != operation {
		return WorkbenchCommand{}, false, ErrConflict
	}
	if record.TombstonedAt != nil {
		return record, true, ErrWorkbenchCommandExpired
	}
	return record, true, nil
}

// WorkbenchCommand loads exact human/source custody. Current-policy authorization
// belongs to its host caller, even when this is a duplicate confirmed receipt.
func (s *Store) WorkbenchCommand(ctx context.Context, scope WorkbenchCommandScope, id string) (WorkbenchCommand, error) {
	if !validWorkbenchScope(scope) || !validWorkbenchID(id) {
		return WorkbenchCommand{}, ErrTransition
	}
	record, err := scanWorkbenchCommand(s.db.QueryRowContext(ctx, "SELECT "+workbenchCommandColumns+" FROM workbench_commands WHERE "+workbenchScopeWhere, workbenchScopeArgs(scope, id)...))
	if err == nil && record.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	return record, err
}

// ClaimWorkbenchCommand is the only accepted->attempting transition. A crash
// after its commit can never grant another effect attempt. claimed=false must
// return the retained receipt/status to the caller, never retry the provider.
func (s *Store) ClaimWorkbenchCommand(ctx context.Context, scope WorkbenchCommandScope, id, digest string, now time.Time) (WorkbenchCommand, bool, error) {
	if !validWorkbenchScope(scope) || !validWorkbenchID(id) || !validWorkbenchDigest(digest) || now.IsZero() {
		return WorkbenchCommand{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkbenchCommand{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := workbenchCommandTx(ctx, tx, scope, id)
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
	result, err := tx.ExecContext(ctx, `UPDATE workbench_commands SET state='attempting',attempted_ns=? WHERE id=? AND state='accepted' AND request_digest=?`, now.UnixNano(), id, digest)
	if err != nil {
		return record, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return record, false, ErrTransition
	}
	record.State = "attempting"
	stamp := now.UTC()
	record.AttemptedAt = &stamp
	return record, true, tx.Commit()
}
