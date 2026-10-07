package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
)

// MaxChildAuthorities bounds active and revoked occurrence grants per gaggle.
// Revocation evidence lasts until the parent settles and child retention ends.
const MaxChildAuthorities = 10000

// ErrChildAuthorityChanged means the caller no longer owns the active attempt.
var ErrChildAuthorityChanged = errors.New("triggerqueue: child stage authority changed")

// ChildAuthority binds a signed grant to the launcher's current stage attempt.
// Only the trusted launcher can bind or replace it after checking the journal.
// Agent requests may present the expected binding but cannot register one.
type ChildAuthority struct {
	ChildParent
	StageOccurrence string
	GrantID         string
	AttemptID       string
	ConfigDigest    string
	PolicyDigest    string
	ExpiresAt       time.Time
	Revoked         bool
}

const childAuthoritySchema = `
CREATE TABLE child_authorities (
 gaggle TEXT NOT NULL, parent_run TEXT NOT NULL, occurrence TEXT NOT NULL,
 grant_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
 config_digest TEXT NOT NULL, policy_digest TEXT NOT NULL,
 expires_ns INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN(0,1)),
 PRIMARY KEY(gaggle,parent_run,occurrence)
);
CREATE INDEX child_authority_retention ON child_authorities(gaggle,parent_run);
`

func (a ChildAuthority) valid() bool {
	return a.ChildParent.valid() && validChildText(a.StageOccurrence, 256, true) &&
		validChildText(a.GrantID, 64, true) && validChildText(a.AttemptID, 128, true) &&
		blobstore.ValidDigest(a.ConfigDigest) && blobstore.ValidDigest(a.PolicyDigest) && !a.ExpiresAt.IsZero()
}

func (a ChildAuthority) keys() []any {
	return []any{a.Gaggle, a.ParentRunID, a.StageOccurrence}
}

type childAuthorityReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readChildAuthority(ctx context.Context, db childAuthorityReader, key ChildAuthority) (ChildAuthority, error) {
	result := ChildAuthority{ChildParent: key.ChildParent, StageOccurrence: key.StageOccurrence}
	var expires int64
	err := db.QueryRowContext(ctx, `SELECT grant_id,attempt_id,config_digest,policy_digest,expires_ns,revoked
 FROM child_authorities WHERE gaggle=? AND parent_run=? AND occurrence=?`, key.keys()...).Scan(
		&result.GrantID, &result.AttemptID, &result.ConfigDigest, &result.PolicyDigest, &expires, &result.Revoked)
	result.ExpiresAt = time.Unix(0, expires).UTC()
	return result, err
}

// ChildAuthority returns trusted launcher custody, including revocation. It is
// an internal read; transport handlers must not expose grants or other stages.
func (s *Store) ChildAuthority(ctx context.Context, parent ChildParent, occurrence string) (ChildAuthority, error) {
	if !parent.valid() || !validChildText(occurrence, 256, true) {
		return ChildAuthority{}, ErrChildAuthorityChanged
	}
	return readChildAuthority(ctx, s.db, ChildAuthority{ChildParent: parent, StageOccurrence: occurrence})
}

// BindChildAuthority installs a launch grant using compare-and-swap. Empty
// expectedGrant requires an absent occurrence; replacement requires its exact
// current grant ID. A revoked attempt cannot renew itself. A replacement attempt
// can rebind the same occurrence after the launcher verifies its new journal ID.
func (s *Store) BindChildAuthority(ctx context.Context, grant ChildAuthority, expectedGrant string, now time.Time) error {
	if !grant.valid() || grant.Revoked || now.IsZero() || !grant.ExpiresAt.After(now) || !validChildText(expectedGrant, 64, false) {
		return ErrChildAuthorityChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureChildParent(ctx, tx, grant.ChildParent, now); err != nil {
		return err
	}
	if err := childParentOpen(ctx, tx, grant.ChildParent); err != nil {
		return err
	}
	current, err := readChildAuthority(ctx, tx, grant)
	if errors.Is(err, sql.ErrNoRows) {
		if expectedGrant != "" {
			return ErrChildAuthorityChanged
		}
		if err := childAuthorityCapacity(ctx, tx, grant.Gaggle); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if sameChildAuthority(current, grant) {
			return nil
		}
		if current.GrantID != expectedGrant || grant.GrantID == current.GrantID || current.Revoked && grant.AttemptID == current.AttemptID {
			return ErrChildAuthorityChanged
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO child_authorities(gaggle,parent_run,occurrence,grant_id,attempt_id,config_digest,policy_digest,expires_ns)
 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(gaggle,parent_run,occurrence) DO UPDATE SET
 grant_id=excluded.grant_id,attempt_id=excluded.attempt_id,config_digest=excluded.config_digest,policy_digest=excluded.policy_digest,expires_ns=excluded.expires_ns,revoked=0`,
		grant.Gaggle, grant.ParentRunID, grant.StageOccurrence, grant.GrantID, grant.AttemptID, grant.ConfigDigest, grant.PolicyDigest, grant.ExpiresAt.UnixNano())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func sameChildAuthority(left, right ChildAuthority) bool {
	return left.ChildParent == right.ChildParent && left.StageOccurrence == right.StageOccurrence &&
		left.GrantID == right.GrantID && left.AttemptID == right.AttemptID && left.ConfigDigest == right.ConfigDigest &&
		left.PolicyDigest == right.PolicyDigest && left.ExpiresAt.Equal(right.ExpiresAt) && left.Revoked == right.Revoked
}

func childParentOpen(ctx context.Context, tx *sql.Tx, parent ChildParent) error {
	var cancelled, settled sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT cancelled_ns,settled_ns FROM child_parents WHERE gaggle=? AND parent_run=?`, parent.Gaggle, parent.ParentRunID).Scan(&cancelled, &settled); err != nil {
		return err
	}
	if cancelled.Valid {
		return ErrParentCancelled
	}
	if settled.Valid {
		return ErrParentSettled
	}
	return nil
}

func childAuthorityCapacity(ctx context.Context, tx *sql.Tx, gaggle string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_authorities WHERE gaggle=?`, gaggle).Scan(&count); err != nil {
		return err
	}
	if count >= MaxChildAuthorities {
		return ErrFull
	}
	return childIntakeCapacity(ctx, tx, gaggle, 0)
}

// RevokeChildAuthority closes one attempt without changing its occurrence or
// child receipt. The retained binding prevents stale launch callbacks renewing
// that attempt. Already-revoked identical bindings are idempotent.
func (s *Store) RevokeChildAuthority(ctx context.Context, grant ChildAuthority) error {
	if !grant.valid() {
		return ErrChildAuthorityChanged
	}
	result, err := s.db.ExecContext(ctx, `UPDATE child_authorities SET revoked=1 WHERE
 gaggle=? AND parent_run=? AND occurrence=? AND grant_id=? AND attempt_id=?`,
		grant.Gaggle, grant.ParentRunID, grant.StageOccurrence, grant.GrantID, grant.AttemptID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrChildAuthorityChanged
	}
	return err
}

// RevokeChildGaggleAuthorities invalidates existing tool credentials before a
// changed gaggle policy is published. The caller must fence concurrent grant
// issuance across revocation and publication. Accepted children retain custody
// and are checked against current execution authority separately. This update
// is bounded by MaxChildAuthorities and creates no additional retained state.
func (s *Store) RevokeChildGaggleAuthorities(ctx context.Context, gaggle string) error {
	if !validChildText(gaggle, 128, true) {
		return ErrChildAuthorityChanged
	}
	_, err := s.db.ExecContext(ctx, `UPDATE child_authorities SET revoked=1 WHERE gaggle=? AND revoked=0`, gaggle)
	return err
}

// CheckChildAuthority verifies an active binding. Acceptance repeats this check
// inside the start transaction, so a revoked/replaced attempt cannot race a start.
func (s *Store) CheckChildAuthority(ctx context.Context, grant ChildAuthority, now time.Time) error {
	return checkChildAuthority(ctx, s.db, grant, now)
}

func checkChildAuthority(ctx context.Context, db childAuthorityReader, grant ChildAuthority, now time.Time) error {
	if !grant.valid() || grant.Revoked || now.IsZero() || !grant.ExpiresAt.After(now) {
		return ErrChildAuthorityChanged
	}
	current, err := readChildAuthority(ctx, db, grant)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChildAuthorityChanged
	}
	if err != nil {
		return err
	}
	if !sameChildAuthority(current, grant) {
		return ErrChildAuthorityChanged
	}
	return nil
}

func pruneChildAuthorities(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM child_authorities WHERE rowid IN (
 SELECT a.rowid FROM child_authorities a JOIN child_parents p USING(gaggle,parent_run)
 WHERE p.settled_ns<=? AND NOT EXISTS(SELECT 1 FROM child_lineages c
 WHERE c.gaggle=a.gaggle AND c.parent_run=a.parent_run AND c.acknowledged_ns IS NULL)
 ORDER BY a.gaggle,a.parent_run,a.occurrence LIMIT ?)`, now.Add(-ChildRetention).UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}
