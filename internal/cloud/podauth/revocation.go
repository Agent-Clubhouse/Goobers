// Package podauth assembles reloadable cloud bearer authentication and durable
// credential-grant revocation without adding cloud dependencies to local auth.
package podauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/controlplane/planes"
)

// RevocationReadTimeout bounds grant admission when PostgreSQL is unavailable.
// There is no replica-local negative cache: every request observes the primary.
const RevocationReadTimeout = 2 * time.Second

// Revocations stores immutable revocation IDs. LiveOwner is read at each write,
// rather than capturing an epoch at construction, so leadership handoffs cannot
// silently reuse an old epoch. Reads also work on follower replicas.
type Revocations struct {
	Pool       *pgxpool.Pool
	InstanceID string
	Lease      *lease.Manager
	LiveOwner  func() *lease.Handle
}

var _ planes.GrantRevocations = (*Revocations)(nil)

// Revoke durably fences one revocation. Repeated writes are idempotent and never
// shorten retention or undo a revocation. IDs are retained until explicit future
// maintenance; expiry alone never makes a previously revoked grant valid again.
func (r *Revocations) Revoke(ctx context.Context, id string, expiresAt time.Time) error {
	if err := r.validate(id); err != nil {
		return err
	}
	if r.Lease == nil || r.LiveOwner == nil || expiresAt.IsZero() {
		return errors.New("cloud grant revocation requires live ownership and expiry")
	}
	owner := r.LiveOwner()
	if owner == nil || owner.InstanceID != r.InstanceID {
		return lease.ErrStale
	}
	return r.Lease.WithFence(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
   INSERT INTO control.grant_revocation (instance_id, grant_id, owner_epoch, expires_at)
   SELECT instance_id, $2, $6, $3 FROM control.lease
   WHERE instance_id=$1 AND name=$4 AND holder=$5 AND epoch=$6
   ON CONFLICT (instance_id, grant_id) DO NOTHING`,
			r.InstanceID, id, expiresAt, owner.Name, owner.Holder, owner.Epoch)
		if err != nil {
			return fmt.Errorf("revoke cloud credential grant: %w", err)
		}
		return nil
	})
}

// IsRevoked checks the primary on every admission. Once Revoke commits, every
// subsequently started check sees it; a check already in flight is bounded by
// RevocationReadTimeout. Errors must never be interpreted as not revoked.
func (r *Revocations) IsRevoked(ctx context.Context, id string) (bool, error) {
	if err := r.validate(id); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, RevocationReadTimeout)
	defer cancel()
	var revoked bool
	err := r.Pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM control.grant_revocation WHERE instance_id=$1 AND grant_id=$2
 )`, r.InstanceID, id).Scan(&revoked)
	if err != nil {
		return false, fmt.Errorf("read cloud grant revocation: %w", err)
	}
	return revoked, nil
}

func (r *Revocations) validate(id string) error {
	if r == nil || r.Pool == nil || strings.TrimSpace(r.InstanceID) == "" || strings.TrimSpace(id) == "" || len(id) > 256 {
		return errors.New("cloud grant revocation requires a pool, instance and bounded grant ID")
	}
	return nil
}
