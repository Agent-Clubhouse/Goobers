package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
)

// Human continuations consume no additional child invocation slots. Their
// retained context and result custody still count against the shared queue cap.
const (
	MaxChildExecutionEpochs   = 8
	MaxChildRestartPlanBytes  = 4 << 20
	childRestartMetadataBytes = 16 << 10
)

const childRestartSchema = `
ALTER TABLE child_lineages ADD COLUMN execution_epoch INTEGER NOT NULL DEFAULT 0 CHECK(execution_epoch BETWEEN 0 AND 8);
ALTER TABLE child_lineages ADD COLUMN execution_run TEXT NOT NULL DEFAULT '';
CREATE TABLE child_execution_epochs (
 child_id TEXT NOT NULL, epoch INTEGER NOT NULL CHECK(epoch BETWEEN 0 AND 8),
 run_id TEXT NOT NULL UNIQUE, source_run TEXT NOT NULL, source_terminal_seq INTEGER NOT NULL,
 source_result_ref TEXT NOT NULL, actor TEXT NOT NULL, stage TEXT NOT NULL,
 plan BLOB NOT NULL CHECK(length(plan)<=4194304), plan_digest TEXT NOT NULL, request_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('queued','running','awaiting_human','completed','failed','cancelled')),
 result_digest TEXT NOT NULL DEFAULT '', result_ref TEXT NOT NULL DEFAULT '', workspace_ref TEXT NOT NULL DEFAULT '',
 accepted_ns INTEGER NOT NULL, updated_ns INTEGER NOT NULL, terminal_ns INTEGER,
 PRIMARY KEY(child_id,epoch)
);
CREATE TABLE child_execution_results (
 child_id TEXT NOT NULL, epoch INTEGER NOT NULL,
 receipt BLOB NOT NULL CHECK(length(receipt) BETWEEN 1 AND 131072),
 receipt_digest TEXT NOT NULL, bundle_digest TEXT NOT NULL,
 bundle BLOB NOT NULL CHECK(length(bundle)<=16777216), PRIMARY KEY(child_id,epoch)
);
CREATE TRIGGER child_release_execution_custody AFTER UPDATE OF tombstoned_ns ON child_lineages
WHEN OLD.tombstoned_ns IS NULL AND NEW.tombstoned_ns IS NOT NULL
BEGIN
 UPDATE child_execution_epochs SET plan=x'' WHERE child_id=OLD.child_id;
 DELETE FROM child_execution_results WHERE child_id=OLD.child_id;
END;
CREATE TRIGGER child_release_execution_history AFTER DELETE ON child_lineages
BEGIN DELETE FROM child_execution_epochs WHERE child_id=OLD.child_id; END;
`

// ChildRestartRequest is host-verified admission, not an HTTP request. Plan is
// the exact bounded common stage-restart snapshot (including selected guidance).
// Parent liveness, current interactive identity, source journal and policy are
// checked by the caller; the database repeats the parent fence and result CAS.
type ChildRestartRequest struct {
	Identity          ChildIdentity
	RunID             string
	SourceRunID       string
	SourceTerminalSeq uint64
	SourceResultRef   string
	Actor             string
	Stage             string
	Plan              []byte
	PlanDigest        string
}

// ChildExecution is retained epoch provenance. Epoch zero is the accepted run;
// later epochs retain their source and request digest without rewriting it.
type ChildExecution struct {
	Identity          ChildIdentity
	Epoch             int
	RunID             string
	SourceRunID       string
	SourceTerminalSeq uint64
	SourceResultRef   string
	Actor             string
	Stage             string
	Plan              []byte
	PlanDigest        string
	RequestDigest     string
	State             ChildState
	ResultRef         string
	WorkspaceRef      string
	AcceptedAt        time.Time
	UpdatedAt         time.Time
	TerminalAt        time.Time
}

func (r ChildRestartRequest) valid(now time.Time) bool {
	return r.Identity.valid() && apiv1.ValidRunID(r.RunID) && len(r.RunID) <= 256 && apiv1.ValidRunID(r.SourceRunID) && len(r.SourceRunID) <= 256 && r.RunID != r.SourceRunID && r.RunID != r.Identity.ParentRunID && r.SourceTerminalSeq > 0 && r.SourceTerminalSeq <= 1<<63-1 && blobstore.ValidDigest(r.SourceResultRef) && validChildText(r.Actor, 1024, true) && validChildText(r.Stage, 256, true) && len(r.Plan) > 0 && len(r.Plan) <= MaxChildRestartPlanBytes && r.PlanDigest == "sha256:"+childDigest(r.Plan) && !now.IsZero()
}
func (r ChildRestartRequest) digest() string {
	r.Plan = nil
	raw, _ := json.Marshal(r)
	return "sha256:" + childDigest(raw)
}

// BeginChildRestart durably selects one new execution for the same unresolved
// child. Identical replay recovers its receipt after cancellation; changed data
// conflicts. Admission and parent cancellation serialize in the same database.
// Acceptance alone does not authorize execution; WithChildExecutionResume is
// the final cancellation/identity fence before an idle runner is released.
func (s *Store) BeginChildRestart(ctx context.Context, r ChildRestartRequest, now time.Time) (ChildExecution, bool, error) {
	if !r.valid(now) {
		return ChildExecution{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChildExecution{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(r.Identity)...))
	if err != nil {
		return ChildExecution{}, false, err
	}
	prior, err := readChildExecution(ctx, tx, c, r.RunID)
	if err == nil {
		if prior.RequestDigest != r.digest() {
			return ChildExecution{}, false, ErrConflict
		}
		return prior, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ChildExecution{}, false, err
	}
	if err = validateChildRestart(ctx, tx, c, r, now); err != nil {
		return ChildExecution{}, false, err
	}
	if err = reserveChildRestart(ctx, tx, c, r); err != nil {
		return ChildExecution{}, false, err
	}
	if c.ExecutionEpoch == 0 {
		if err = archiveInitialChildExecution(ctx, tx, c, r.SourceResultRef); err != nil {
			return ChildExecution{}, false, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO child_execution_epochs(child_id,epoch,run_id,source_run,source_terminal_seq,source_result_ref,actor,stage,plan,plan_digest,request_digest,state,accepted_ns,updated_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,'queued',?,?)`, c.ChildID, c.ExecutionEpoch+1, r.RunID, r.SourceRunID, r.SourceTerminalSeq, r.SourceResultRef, r.Actor, r.Stage, r.Plan, r.PlanDigest, r.digest(), now.UnixNano(), now.UnixNano())
	if err != nil {
		return ChildExecution{}, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE child_lineages SET execution_epoch=execution_epoch+1,execution_run=?,state='queued',result_ref='',workspace_ref='',terminal_ns=NULL,updated_ns=? WHERE child_id=?`, r.RunID, now.UnixNano(), c.ChildID)
	if err != nil {
		return ChildExecution{}, false, err
	}
	result, err := readChildExecution(ctx, tx, c, r.RunID)
	if err != nil {
		return ChildExecution{}, false, err
	}
	return result, false, tx.Commit()
}

func validateChildRestart(ctx context.Context, tx *sql.Tx, c ChildRecord, r ChildRestartRequest, now time.Time) error {
	if err := childParentOpen(ctx, tx, c.Identity.ChildParent); err != nil {
		return err
	}
	if !c.TombstonedAt.IsZero() || !c.AcknowledgedAt.IsZero() || c.ActiveRunID() != r.SourceRunID || c.RunID == r.RunID || now.Before(c.UpdatedAt) {
		return ErrTransition
	}
	if c.State != ChildFailed && c.State != ChildAwaitingHuman {
		return ErrTransition
	}
	if err := validateCurrentChildExecution(ctx, tx, c); err != nil {
		return err
	}
	result, err := readChildResult(ctx, tx, c)
	if err != nil {
		return err
	}
	if result.ReceiptDigest != r.SourceResultRef || (c.State == ChildFailed && result.ReceiptDigest != c.ResultRef) {
		return ErrChildResultUnavailable
	}
	if c.ExecutionEpoch >= MaxChildExecutionEpochs {
		return ErrChildLimit
	}
	var knownRun bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM child_lineages WHERE acceptance_id=?)`, "trigger-"+r.RunID).Scan(&knownRun); err != nil {
		return err
	}
	if knownRun {
		return ErrConflict
	}
	var disposition string
	if err := tx.QueryRowContext(ctx, `SELECT disposition_digest FROM child_lineages WHERE child_id=?`, c.ChildID).Scan(&disposition); err != nil {
		return err
	}
	if disposition != "" {
		return ErrTransition
	}
	return nil
}
func reserveChildRestart(ctx context.Context, tx *sql.Tx, c ChildRecord, r ChildRestartRequest) error {
	if err := childByteCapacity(ctx, tx, len(r.Plan)+childRestartMetadataBytes+childResultAllowance); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE child_lineages SET reserved_bytes=reserved_bytes+? WHERE child_id=?`, childResultAllowance, c.ChildID)
	return err
}
func archiveInitialChildExecution(ctx context.Context, tx *sql.Tx, c ChildRecord, sourceResult string) error {
	var terminal any
	if !c.TerminalAt.IsZero() {
		terminal = c.TerminalAt.UnixNano()
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO child_execution_epochs(child_id,epoch,run_id,source_run,source_terminal_seq,source_result_ref,actor,stage,plan,plan_digest,request_digest,state,result_ref,workspace_ref,accepted_ns,updated_ns,terminal_ns) VALUES(?,0,?,'',0,'','','',x'','','',?,?,?,?,?,?)`, c.ChildID, c.RunID, c.State, sourceResult, c.WorkspaceRef, c.AcceptedAt.UnixNano(), c.UpdatedAt.UnixNano(), terminal)
	return err
}
