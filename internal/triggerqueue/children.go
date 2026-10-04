package triggerqueue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Child lineage is execution custody, stored in the same transaction as its
// ordinary trigger receipt. These internal APIs require the caller to verify
// live run/attempt authority, policy and artifact ownership before calling them.
// They do not authorize a tool caller, execute a workflow or stop a process.
const (
	MaxChildrenPerOccurrence = 32
	MaxChildLineages         = 10000
	MaxChildTombstones       = 100000
	MaxChildRefBytes         = 2048
	ChildRetention           = 30 * 24 * time.Hour
	ChildTombstoneRetention  = 30 * 24 * time.Hour
	childStoreByteCeiling    = 256 << 20
)

// Child admission errors distinguish an occupied slot, a spent allowance and
// closed parent authority from ordinary queue capacity and payload conflicts.
var (
	ErrChildSlotOccupied = errors.New("triggerqueue: stage occurrence has an unresolved child")
	ErrChildLimit        = errors.New("triggerqueue: stage occurrence child limit reached")
	ErrParentCancelled   = errors.New("triggerqueue: child parent cancellation is fenced")
	ErrParentSettled     = errors.New("triggerqueue: child parent is settled")
)

// ChildParent is qualified by gaggle even when run IDs happen to be globally
// unique. A child reference never grants cross-gaggle access.
type ChildParent struct {
	Gaggle      string
	ParentRunID string
}

// ChildIdentity survives attempts, reconnects and process restarts. Occurrence
// includes the durable branch/loop visit; it must not be derived from attempt ID.
type ChildIdentity struct {
	ChildParent
	StageOccurrence string
	InvocationKey   string
}

// ChildAcceptance carries the existing ordinary start-envelope encoding,
// containing verified proposal/snapshot references, not inline proposal bytes.
// MaxChildren is the validated, pinned occurrence ceiling (1..32).
type ChildAcceptance struct {
	Identity    ChildIdentity
	Actor       string
	Payload     []byte
	MaxChildren int
}

// ChildState describes observed child custody, independently of queue claims.
type ChildState string

// Child lifecycle states retain the occurrence slot until acknowledgement.
const (
	ChildQueued        ChildState = "queued"
	ChildRunning       ChildState = "running"
	ChildAwaitingHuman ChildState = "awaiting_human"
	ChildCompleted     ChildState = "completed"
	ChildFailed        ChildState = "failed"
	ChildCancelled     ChildState = "cancelled"
)

// Terminal reports whether execution has a verified final result.
func (s ChildState) Terminal() bool {
	return s == ChildCompleted || s == ChildFailed || s == ChildCancelled
}

// ChildRecord is a lineage reference. TombstonedAt means full result/start
// custody has expired; callers must not present it as a recoverable result.
type ChildRecord struct {
	Identity              ChildIdentity
	Sequence              int
	ChildID               string
	AcceptanceID          string
	StartKey              string
	RunID                 string
	State                 ChildState
	ResultRef             string
	WorkspaceRef          string
	AcceptedAt            time.Time
	UpdatedAt             time.Time
	TerminalAt            time.Time
	AcknowledgedAt        time.Time
	TombstonedAt          time.Time
	CancellationRequested bool
}

// ChildStateUpdate is a compare-and-swap, not an instruction to execute effects.
// Terminal outcomes require a verified result reference, including cancellation.
type ChildStateUpdate struct {
	Expected     ChildState
	State        ChildState
	ResultRef    string
	WorkspaceRef string
}

const childSchema = `
CREATE TABLE child_parents (
 gaggle TEXT NOT NULL, parent_run TEXT NOT NULL,
 cancelled_ns INTEGER, settled_ns INTEGER, cancel_actor TEXT NOT NULL DEFAULT '',
 created_ns INTEGER NOT NULL,
 PRIMARY KEY(gaggle,parent_run)
);
CREATE TABLE child_occurrences (
 gaggle TEXT NOT NULL, parent_run TEXT NOT NULL, occurrence TEXT NOT NULL,
 accepted_count INTEGER NOT NULL CHECK(accepted_count BETWEEN 0 AND 32),
 max_children INTEGER NOT NULL CHECK(max_children BETWEEN 1 AND 32),
 PRIMARY KEY(gaggle,parent_run,occurrence)
);
CREATE TABLE child_lineages (
 gaggle TEXT NOT NULL, parent_run TEXT NOT NULL, occurrence TEXT NOT NULL, invocation_key TEXT NOT NULL,
 sequence INTEGER NOT NULL, child_id TEXT NOT NULL UNIQUE, acceptance_id TEXT NOT NULL UNIQUE,
 start_key TEXT NOT NULL UNIQUE, actor_digest TEXT NOT NULL, payload_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('queued','running','awaiting_human','completed','failed','cancelled')),
 result_ref TEXT NOT NULL DEFAULT '', workspace_ref TEXT NOT NULL DEFAULT '',
 accepted_ns INTEGER NOT NULL, updated_ns INTEGER NOT NULL, terminal_ns INTEGER, acknowledged_ns INTEGER,
 tombstoned_ns INTEGER,
 PRIMARY KEY(gaggle,parent_run,occurrence,invocation_key)
);
CREATE UNIQUE INDEX child_one_unresolved ON child_lineages(gaggle,parent_run,occurrence) WHERE acknowledged_ns IS NULL;
CREATE INDEX child_parent_page ON child_lineages(gaggle,parent_run,child_id);
CREATE INDEX child_retention ON child_lineages(gaggle,tombstoned_ns,acknowledged_ns,child_id);
CREATE INDEX child_parent_retention ON child_parents(gaggle,settled_ns,parent_run);
-- Ordinary Accept's existing pruning query must not erase a child's receipt.
-- Tombstoning removes this pin before the dedicated pruner deletes the receipt.
CREATE TRIGGER child_pin_start BEFORE DELETE ON triggers
WHEN EXISTS(SELECT 1 FROM child_lineages WHERE acceptance_id=OLD.id AND tombstoned_ns IS NULL)
BEGIN SELECT RAISE(IGNORE); END;
-- This is a last boundary guard for existing BeginDispatch. The fence and
-- acceptance are serialized in this database, not in a separate cancel store.
CREATE TRIGGER child_cancel_dispatch BEFORE UPDATE OF state ON triggers
WHEN NEW.state='dispatching' AND OLD.state='accepted' AND EXISTS(
 SELECT 1 FROM child_lineages c JOIN child_parents p USING(gaggle,parent_run)
 WHERE c.acceptance_id=OLD.id AND (p.cancelled_ns IS NOT NULL OR c.state IN ('completed','failed','cancelled')))
BEGIN SELECT RAISE(IGNORE); END;
-- Recovery may prove that a claimed start never began. Preserve that proof as
-- rejection when a cancellation/terminal fence already forbids another start.
CREATE TRIGGER child_fenced_requeue AFTER UPDATE OF state ON triggers
WHEN NEW.state='accepted' AND OLD.state='dispatching' AND EXISTS(
 SELECT 1 FROM child_lineages c JOIN child_parents p USING(gaggle,parent_run)
 WHERE c.acceptance_id=NEW.id AND (p.cancelled_ns IS NOT NULL OR c.state IN ('completed','failed','cancelled')))
BEGIN
 UPDATE triggers SET state='rejected',run_id='',reason='child start fenced',finished_ns=(
  SELECT MAX(NEW.accepted_ns,c.updated_ns,COALESCE(p.cancelled_ns,0))
  FROM child_lineages c JOIN child_parents p USING(gaggle,parent_run) WHERE c.acceptance_id=NEW.id)
 WHERE id=NEW.id;
END;
`

func validChildText(value string, max int, required bool) bool {
	if len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value || (required && value == "") {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (p ChildParent) valid() bool {
	return validChildText(p.Gaggle, 128, true) && validChildText(p.ParentRunID, 256, true)
}

func (i ChildIdentity) valid() bool {
	return i.ChildParent.valid() && validChildText(i.StageOccurrence, 256, true) && validChildText(i.InvocationKey, 256, true)
}

func childDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func childStartKey(i ChildIdentity) string {
	// An array avoids ambiguous delimiter concatenation and field-name changes.
	b, _ := json.Marshal([4]string{i.Gaggle, i.ParentRunID, i.StageOccurrence, i.InvocationKey})
	return "haw-child:v1:" + childDigest(b)
}

const childColumns = `c.gaggle,c.parent_run,c.occurrence,c.invocation_key,c.sequence,c.child_id,c.acceptance_id,c.start_key,c.state,c.result_ref,c.workspace_ref,c.accepted_ns,c.updated_ns,c.terminal_ns,c.acknowledged_ns,c.tombstoned_ns,p.cancelled_ns IS NOT NULL`
const childFrom = ` FROM child_lineages c JOIN child_parents p USING(gaggle,parent_run)`
const childWhere = ` WHERE c.gaggle=? AND c.parent_run=? AND c.occurrence=? AND c.invocation_key=?`

func childArgs(i ChildIdentity) []any {
	return []any{i.Gaggle, i.ParentRunID, i.StageOccurrence, i.InvocationKey}
}

func scanChild(row scanner) (ChildRecord, error) {
	var c ChildRecord
	var accepted, updated int64
	var terminal, ack, tombstone sql.NullInt64
	err := row.Scan(&c.Identity.Gaggle, &c.Identity.ParentRunID, &c.Identity.StageOccurrence, &c.Identity.InvocationKey,
		&c.Sequence, &c.ChildID, &c.AcceptanceID, &c.StartKey, &c.State, &c.ResultRef, &c.WorkspaceRef,
		&accepted, &updated, &terminal, &ack, &tombstone, &c.CancellationRequested)
	if err != nil {
		return ChildRecord{}, err
	}
	c.RunID = strings.TrimPrefix(c.AcceptanceID, "trigger-")
	c.AcceptedAt, c.UpdatedAt = time.Unix(0, accepted).UTC(), time.Unix(0, updated).UTC()
	if terminal.Valid {
		c.TerminalAt = time.Unix(0, terminal.Int64).UTC()
	}
	if ack.Valid {
		c.AcknowledgedAt = time.Unix(0, ack.Int64).UTC()
	}
	if tombstone.Valid {
		c.TombstonedAt = time.Unix(0, tombstone.Int64).UTC()
	}
	return c, nil
}

// GetChild is an internal lookup. HTTP/tool callers must authorize the parent
// and current occurrence first; supplying a child identity is not authorization.
func (s *Store) GetChild(ctx context.Context, identity ChildIdentity) (ChildRecord, error) {
	if !identity.valid() {
		return ChildRecord{}, errors.New("triggerqueue: invalid child identity")
	}
	return scanChild(s.db.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(identity)...))
}

// AcceptChild atomically accepts ordinary queue custody, lineage, the occurrence
// slot, and its monotonically increasing count. Duplicate lookup precedes fences:
// an authorized retry can recover an old receipt after cancellation, never start anew.
func (s *Store) AcceptChild(ctx context.Context, req ChildAcceptance, now time.Time) (ChildRecord, bool, error) {
	if err := req.validate(now); err != nil {
		return ChildRecord{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChildRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	identity := req.Identity
	var actorDigest, payloadDigest string
	err = tx.QueryRowContext(ctx, `SELECT actor_digest,payload_digest FROM child_lineages c`+childWhere, childArgs(identity)...).Scan(&actorDigest, &payloadDigest)
	if err == nil {
		if actorDigest != childDigest([]byte(req.Actor)) || payloadDigest != childDigest(req.Payload) {
			return ChildRecord{}, false, ErrConflict
		}
		c, readErr := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(identity)...))
		return c, true, readErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ChildRecord{}, false, err
	}
	if err = childIntakeCapacity(ctx, tx, identity.Gaggle); err != nil {
		return ChildRecord{}, false, err
	}
	sequence, err := reserveChildOccurrence(ctx, tx, identity, req.MaxChildren, now)
	if err != nil {
		return ChildRecord{}, false, err
	}
	startKey := childStartKey(identity)
	var existing int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM triggers WHERE key=?`, startKey).Scan(&existing); err != nil {
		return ChildRecord{}, false, err
	}
	if existing != 0 {
		return ChildRecord{}, false, ErrConflict
	}
	runID := fmt.Sprintf("%x", randomID())
	acceptanceID := "trigger-" + runID
	if _, err = tx.ExecContext(ctx, `INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES(?,?,?,?,?,?)`, acceptanceID, startKey, req.Actor, req.Payload, Accepted, now.UnixNano()); err != nil {
		return ChildRecord{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO child_lineages(gaggle,parent_run,occurrence,invocation_key,sequence,child_id,acceptance_id,start_key,actor_digest,payload_digest,state,accepted_ns,updated_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, identity.Gaggle, identity.ParentRunID, identity.StageOccurrence, identity.InvocationKey, sequence, "child-"+runID, acceptanceID, startKey, childDigest([]byte(req.Actor)), childDigest(req.Payload), ChildQueued, now.UnixNano(), now.UnixNano()); err != nil {
		return ChildRecord{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_occurrences SET accepted_count=accepted_count+1 WHERE gaggle=? AND parent_run=? AND occurrence=?`, identity.Gaggle, identity.ParentRunID, identity.StageOccurrence); err != nil {
		return ChildRecord{}, false, err
	}
	c, err := scanChild(tx.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(identity)...))
	if err != nil {
		return ChildRecord{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return ChildRecord{}, false, err
	}
	return c, false, nil
}

func (req ChildAcceptance) validate(now time.Time) error {
	if !req.Identity.valid() || !validChildText(req.Actor, 1024, true) || len(req.Payload) == 0 || len(req.Payload) > MaxPayloadBytes || req.MaxChildren < 1 || req.MaxChildren > MaxChildrenPerOccurrence || now.IsZero() {
		return errors.New("triggerqueue: invalid child acceptance")
	}
	return nil
}

func reserveChildOccurrence(ctx context.Context, tx *sql.Tx, identity ChildIdentity, maxChildren int, now time.Time) (int, error) {
	if err := ensureChildParent(ctx, tx, identity.ChildParent, now); err != nil {
		return 0, err
	}
	var cancelled, settled sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT cancelled_ns,settled_ns FROM child_parents WHERE gaggle=? AND parent_run=?`, identity.Gaggle, identity.ParentRunID).Scan(&cancelled, &settled); err != nil {
		return 0, err
	}
	if cancelled.Valid {
		return 0, ErrParentCancelled
	}
	if settled.Valid {
		return 0, ErrParentSettled
	}
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_lineages WHERE gaggle=? AND parent_run=? AND occurrence=? AND acknowledged_ns IS NULL`, identity.Gaggle, identity.ParentRunID, identity.StageOccurrence).Scan(&unresolved); err != nil {
		return 0, err
	}
	if unresolved != 0 {
		return 0, ErrChildSlotOccupied
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO child_occurrences(gaggle,parent_run,occurrence,accepted_count,max_children) VALUES(?,?,?,0,?) ON CONFLICT DO NOTHING`, identity.Gaggle, identity.ParentRunID, identity.StageOccurrence, maxChildren); err != nil {
		return 0, err
	}
	var count, ceiling int
	if err := tx.QueryRowContext(ctx, `SELECT accepted_count,max_children FROM child_occurrences WHERE gaggle=? AND parent_run=? AND occurrence=?`, identity.Gaggle, identity.ParentRunID, identity.StageOccurrence).Scan(&count, &ceiling); err != nil {
		return 0, err
	}
	// A caller cannot widen the first accepted policy; a tighter current policy
	// can prevent more children without changing earlier receipt identities.
	if count >= min(ceiling, maxChildren) {
		return 0, ErrChildLimit
	}
	return count + 1, nil
}

func childIntakeCapacity(ctx context.Context, tx *sql.Tx, gaggle string) error {
	var starts, lineages, tombstones int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM triggers`).Scan(&starts); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER(WHERE tombstoned_ns IS NULL),COUNT(*) FILTER(WHERE tombstoned_ns IS NOT NULL) FROM child_lineages WHERE gaggle=?`, gaggle).Scan(&lineages, &tombstones); err != nil {
		return err
	}
	if starts >= MaxRecords || lineages >= MaxChildLineages || tombstones >= MaxChildTombstones {
		return ErrFull
	}
	var pages, freePages, pageSize int64
	if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&freePages); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return err
	}
	// The existing database has a 256 MiB hard ceiling. Preserve 20% for
	// transitions, cancellation and maintenance; do not add a second reserve.
	if (pages-freePages)*pageSize+MaxPayloadBytes+32*1024 > childStoreByteCeiling*4/5 {
		return ErrFull
	}
	return nil
}

func ensureChildParent(ctx context.Context, tx *sql.Tx, parent ChildParent, now time.Time) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_parents WHERE gaggle=?`, parent.Gaggle).Scan(&count); err != nil {
		return err
	}
	if count >= MaxChildLineages {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_parents WHERE gaggle=? AND parent_run=?`, parent.Gaggle, parent.ParentRunID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrFull
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO child_parents(gaggle,parent_run,created_ns) VALUES(?,?,?) ON CONFLICT DO NOTHING`, parent.Gaggle, parent.ParentRunID, now.UnixNano())
	return err
}
