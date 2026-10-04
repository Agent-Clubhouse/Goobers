package triggerqueue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Publication bounds apply before retaining either desired intent or observed effects.
const (
	MaxChildPublicationIntentBytes  = 64 << 10
	MaxChildPublicationReceiptBytes = 16 << 10
)

const childPublicationSchema = `
ALTER TABLE child_lineages ADD COLUMN publication_pending INTEGER NOT NULL DEFAULT 0 CHECK(publication_pending BETWEEN 0 AND 2);
ALTER TABLE child_lineages ADD COLUMN publication_branch_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE child_lineages ADD COLUMN publication_pr_digest TEXT NOT NULL DEFAULT '';
CREATE TABLE child_publications (
 child_id TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('branch','pr')),
 intent BLOB NOT NULL CHECK(length(intent) BETWEEN 1 AND 65536),
 digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('prepared','effect_pending','confirmed')),
 receipt BLOB NOT NULL CHECK(length(receipt)<=16384),
 receipt_digest TEXT NOT NULL DEFAULT '',
 created_ns INTEGER NOT NULL, updated_ns INTEGER NOT NULL,
 PRIMARY KEY(child_id,action)
);
CREATE TRIGGER child_release_publications AFTER UPDATE OF tombstoned_ns ON child_lineages
WHEN OLD.tombstoned_ns IS NULL AND NEW.tombstoned_ns IS NOT NULL
BEGIN DELETE FROM child_publications WHERE child_id=OLD.child_id; END;
`

// ErrChildPublicationPending means no publication was admitted for this action.
var ErrChildPublicationPending = errors.New("triggerqueue: child publication intent missing")

// ErrChildPublicationUnavailable refuses missing or corrupt admitted publication custody.
var ErrChildPublicationUnavailable = errors.New("triggerqueue: child publication custody unavailable")

// ChildPublication separates the immutable desired action from its observed receipt.
type ChildPublication struct {
	Identity      ChildIdentity
	Action        string
	Intent        []byte
	Digest        string
	State         string
	Receipt       []byte
	ReceiptDigest string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func validPublicationAction(action string) bool { return action == "branch" || action == "pr" }

// PrepareChildPublication retains one immutable intent per action and child.
// It reserves receipt capacity before any provider effect; lineage pruning owns
// both rows, so retries cannot grow an independent unbounded publication log.
func (s *Store) PrepareChildPublication(ctx context.Context, id ChildIdentity, action string, intent []byte) (ChildPublication, error) {
	if !id.valid() || !validPublicationAction(action) || len(intent) == 0 || len(intent) > MaxChildPublicationIntentBytes || !json.Valid(intent) {
		return ChildPublication{}, ErrChildPublicationUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChildPublication{}, err
	}
	defer func() { _ = tx.Rollback() }()
	child, err := publicationChild(ctx, tx, id)
	if err != nil {
		return ChildPublication{}, err
	}
	current, err := readChildPublication(ctx, tx, child, action)
	if err == nil {
		if !bytes.Equal(current.Intent, intent) {
			return ChildPublication{}, ErrConflict
		}
		return current, tx.Commit()
	}
	if !errors.Is(err, ErrChildPublicationPending) {
		return ChildPublication{}, err
	}
	if err = publicationParentOpen(ctx, tx, child); err != nil {
		return ChildPublication{}, err
	}
	if err = childByteCapacity(ctx, tx, len(intent)+MaxChildPublicationReceiptBytes+4096); err != nil {
		return ChildPublication{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_lineages SET reserved_bytes=reserved_bytes+?, `+publicationSelector(action)+`=? WHERE child_id=?`, MaxChildPublicationReceiptBytes+1024, "sha256:"+childDigest(intent), child.ChildID); err != nil {
		return ChildPublication{}, err
	}
	now := time.Now().UTC()
	current = ChildPublication{CreatedAt: now, UpdatedAt: now, Identity: id, Action: action, Intent: bytes.Clone(intent), Digest: "sha256:" + childDigest(intent), State: "prepared", Receipt: []byte{}}
	if _, err = tx.ExecContext(ctx, `INSERT INTO child_publications(child_id,action,intent,digest,state,receipt,created_ns,updated_ns) VALUES(?,?,?,?,?,?,?,?)`, child.ChildID, action, intent, current.Digest, current.State, current.Receipt, now.UnixNano(), now.UnixNano()); err != nil {
		return ChildPublication{}, err
	}
	return current, tx.Commit()
}

func publicationChild(ctx context.Context, reader childProposalReader, id ChildIdentity) (ChildRecord, error) {
	child, err := scanChild(reader.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+childWhere, childArgs(id)...))
	if err != nil {
		return child, err
	}
	if !child.TombstonedAt.IsZero() {
		return child, ErrChildPublicationUnavailable
	}
	return child, nil
}
func publicationParentOpen(ctx context.Context, tx *sql.Tx, child ChildRecord) error {
	if err := childParentOpen(ctx, tx, child.Identity.ChildParent); err != nil {
		return err
	}
	if child.State.Terminal() || child.CancellationRequested || !child.AcknowledgedAt.IsZero() {
		return ErrTransition
	}
	return nil
}

func readChildPublication(ctx context.Context, reader childProposalReader, child ChildRecord, action string) (ChildPublication, error) {
	out := ChildPublication{Identity: child.Identity, Action: action}
	var selector string
	if err := reader.QueryRowContext(ctx, `SELECT `+publicationSelector(action)+` FROM child_lineages WHERE child_id=?`, child.ChildID).Scan(&selector); err != nil {
		return out, err
	}
	var created, updated int64
	err := reader.QueryRowContext(ctx, `SELECT intent,digest,state,receipt,receipt_digest,created_ns,updated_ns FROM child_publications WHERE child_id=? AND action=? AND length(intent)<=? AND length(receipt)<=?`, child.ChildID, action, MaxChildPublicationIntentBytes, MaxChildPublicationReceiptBytes).Scan(&out.Intent, &out.Digest, &out.State, &out.Receipt, &out.ReceiptDigest, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		if selector != "" {
			return out, ErrChildPublicationUnavailable
		}
		return out, ErrChildPublicationPending
	}
	if err != nil {
		return out, err
	}
	if created <= 0 || updated < created {
		return out, ErrChildPublicationUnavailable
	}
	out.CreatedAt, out.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	if selector == "" || selector != out.Digest || out.Digest != "sha256:"+childDigest(out.Intent) || !json.Valid(out.Intent) || (out.State == "confirmed" && (!json.Valid(out.Receipt) || out.ReceiptDigest != "sha256:"+childDigest(out.Receipt))) {
		return out, ErrChildPublicationUnavailable
	}
	return out, nil
}

// ChildPublication reads verified intent and confirmed evidence from owned custody.
func (s *Store) ChildPublication(ctx context.Context, id ChildIdentity, action string) (ChildPublication, error) {
	if !id.valid() || !validPublicationAction(action) {
		return ChildPublication{}, ErrChildPublicationUnavailable
	}
	child, err := publicationChild(ctx, s.db, id)
	if err != nil {
		return ChildPublication{}, err
	}
	return readChildPublication(ctx, s.db, child, action)
}

// BeginChildPublicationEffect follows a verified preflight. A pending effect is
// uncertainty: callers must reconcile its exact target, never allocate another.
func (s *Store) BeginChildPublicationEffect(ctx context.Context, expected ChildPublication) error {
	return s.changeChildPublication(ctx, expected, nil)
}

// ConfirmChildPublication stores observed effect evidence after the network
// call. Parent cancellation does not erase effects that already happened.
func (s *Store) ConfirmChildPublication(ctx context.Context, expected ChildPublication, receipt []byte) error {
	if len(receipt) == 0 || len(receipt) > MaxChildPublicationReceiptBytes || !json.Valid(receipt) {
		return ErrChildPublicationUnavailable
	}
	return s.changeChildPublication(ctx, expected, receipt)
}

func (s *Store) changeChildPublication(ctx context.Context, expected ChildPublication, receipt []byte) error {
	if !expected.Identity.valid() || !validPublicationAction(expected.Action) {
		return ErrChildPublicationUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	child, err := publicationChild(ctx, tx, expected.Identity)
	if err != nil {
		return err
	}
	current, err := readChildPublication(ctx, tx, child, expected.Action)
	if err != nil {
		return err
	}
	if current.Digest != expected.Digest || !bytes.Equal(current.Intent, expected.Intent) {
		return ErrConflict
	}
	pendingDelta := 0
	state := "effect_pending"
	if receipt == nil {
		if err = publicationParentOpen(ctx, tx, child); err != nil {
			return err
		}
		if current.State != "prepared" && current.State != "effect_pending" {
			return ErrTransition
		}
		if current.State == "prepared" {
			pendingDelta = 1
		}
		receipt = []byte{}
	} else {
		state = "confirmed"
		if current.State == "confirmed" {
			if !bytes.Equal(current.Receipt, receipt) {
				return ErrConflict
			}
			return tx.Commit()
		}
		if current.State != "effect_pending" {
			return ErrTransition
		}
		pendingDelta = -1
		if err = consumeChildStorage(ctx, tx, child.ChildID, MaxChildPublicationReceiptBytes+1024, len(receipt)+1024); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_lineages SET publication_pending=publication_pending+? WHERE child_id=?`, pendingDelta, child.ChildID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE child_publications SET state=?,receipt=?,receipt_digest=?,updated_ns=MAX(updated_ns,?) WHERE child_id=? AND action=?`, state, receipt, publicationReceiptDigest(receipt), time.Now().UTC().UnixNano(), child.ChildID, expected.Action); err != nil {
		return err
	}
	return tx.Commit()
}

func publicationReceiptDigest(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return "sha256:" + childDigest(data)
}

func publicationSelector(action string) string {
	if action == "branch" {
		return "publication_branch_digest"
	}
	return "publication_pr_digest"
}
