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
	shared "github.com/goobers/goobers/internal/podauth"
)

// RevocationReadTimeout bounds grant admission when PostgreSQL is unavailable.
// There is no replica-local negative cache: every request observes the primary.
const RevocationReadTimeout = 2 * time.Second

// Revocations stores immutable revocation IDs on the API pool. A revocation
// takes effect immediately on any replica and does not require controller ownership.
type Revocations struct {
	Pool       *pgxpool.Pool
	InstanceID string
}

var _ GrantAdmission = (*Revocations)(nil)

// Revoke appends one monotonic security fact. Repeated writes are idempotent
// and never shorten retention or undo a revocation. Only the controller can
// delete a row after the signed grant expires.
func (r *Revocations) Revoke(ctx context.Context, id string, expiresAt time.Time) error {
	if err := r.validate(id); err != nil {
		return err
	}
	if expiresAt.IsZero() {
		return errors.New("cloud grant revocation requires expiry")
	}
	_, err := r.Pool.Exec(ctx, `INSERT INTO control.grant_revocation (instance_id,grant_id,expires_at)
 VALUES($1,$2,$3) ON CONFLICT(instance_id,grant_id) DO NOTHING`, r.InstanceID, id, expiresAt)
	if err != nil {
		return fmt.Errorf("revoke cloud credential grant: %w", err)
	}
	return nil
}

// IsRevoked checks the primary on every admission. Once Revoke commits, every
// subsequently started check sees it; a check already in flight is bounded by
// RevocationReadTimeout. Errors must never be interpreted as not revoked.
func (r *Revocations) IsRevoked(ctx context.Context, id string) (bool, error) {
	revoked, _, err := r.readRevocation(ctx, id)
	return revoked, err
}

// CheckGrant rejects expired or revoked grants using a single primary read.
// Comparing to the database clock makes cleanup safe even when the API host's
// wall clock trails the database. The signed expiry is supplied by Auth only
// after signature verification; the DB read must succeed before admission.
func (r *Revocations) CheckGrant(ctx context.Context, id string, expiresAt time.Time) error {
	revoked, now, err := r.readRevocation(ctx, id)
	if err != nil {
		return err
	}
	if revoked {
		return shared.ErrInvalidCredentialGrant
	}
	if !expiresAt.After(now) {
		return shared.ErrExpiredCredentialGrant
	}
	return nil
}

func (r *Revocations) readRevocation(ctx context.Context, id string) (bool, time.Time, error) {
	if err := r.validate(id); err != nil {
		return false, time.Time{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, RevocationReadTimeout)
	defer cancel()
	var revoked bool
	var now time.Time
	err := r.Pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM control.grant_revocation WHERE instance_id=$1 AND grant_id=$2
 ), clock_timestamp()`, r.InstanceID, id).Scan(&revoked, &now)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("read cloud grant revocation: %w", err)
	}
	return revoked, now, nil
}

func (r *Revocations) validate(id string) error {
	if r == nil || r.Pool == nil || strings.TrimSpace(r.InstanceID) == "" || strings.TrimSpace(id) == "" || len(id) > 256 {
		return errors.New("cloud grant revocation requires a pool, instance and bounded grant ID")
	}
	return nil
}

// PruneExpired is controller-only maintenance. The epoch lock prevents a stale
// replica from deleting security facts after losing authority. Reads use the DB
// clock, as does CheckGrant, so cleanup cannot resurrect a revoked grant on
// an API replica whose wall clock trails the database.
func PruneExpired(ctx context.Context, manager *lease.Manager, handle *lease.Handle) error {
	if manager == nil || handle == nil {
		return errors.New("grant revocation cleanup requires controller ownership")
	}
	return manager.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		// DELETE takes the needed row locks. A SELECT FOR UPDATE here would
		// require UPDATE authority over immutable security facts.
		_, err := tx.Exec(ctx, `DELETE FROM control.grant_revocation WHERE instance_id=$1 AND grant_id IN (
   SELECT grant_id FROM control.grant_revocation WHERE instance_id=$1 AND expires_at<clock_timestamp()
   ORDER BY expires_at,grant_id LIMIT 256)`, handle.InstanceID)
		return err
	})
}
