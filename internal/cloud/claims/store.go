// Package claims implements PostgreSQL claim and scheduler-state custody.
package claims

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/claimrelease"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/localscheduler"
)

const operationTimeout = 2 * time.Second

// MaxClaims bounds active rows per instance.
const MaxClaims = 10000

// ErrCapacity means a new key would exceed the active claim bound.
var (
	ErrCapacity   = errors.New("cloud claims: active claim capacity exhausted")
	ErrConflict   = errors.New("cloud claims: key belongs to another owner")
	ErrTransition = errors.New("cloud claims: stale state transition")
)

// Store is one instance's claim ledger and scheduler state.
type Store struct {
	pool       *pgxpool.Pool
	instanceID string
	manager    *cloudlease.Manager
	handle     *cloudlease.Handle
}

// Services is the cloud scheduler's shared claim and state contract.
type Services interface {
	localscheduler.ClaimStore
	localscheduler.SchedulerState
}

var _ Services = (*Store)(nil)

// New constructs a store bound to a live control lease.
func New(
	pool *pgxpool.Pool,
	instanceID string,
	manager *cloudlease.Manager,
	handle *cloudlease.Handle,
) (*Store, error) {
	if pool == nil || strings.TrimSpace(instanceID) == "" || manager == nil || handle == nil {
		return nil, errors.New("cloud claims require a pool, instance, and live lease")
	}
	if instanceID != handle.InstanceID {
		return nil, errors.New("cloud claims instance does not match lease handle")
	}
	return &Store{pool: pool, instanceID: instanceID, manager: manager, handle: handle}, nil
}

// Claim acquires a legacy unscoped claim.
func (s *Store) Claim(itemID, runID, workflow string, lease time.Duration) (bool, string, error) {
	return s.ClaimScoped(
		localscheduler.ClaimKey{Gaggle: "legacy", Provider: "legacy", ExternalID: itemID},
		runID, workflow, lease,
	)
}

// ClaimScoped acquires or idempotently renews one scoped claim.
func (s *Store) ClaimScoped(
	key localscheduler.ClaimKey,
	runID, workflow string,
	lease time.Duration,
) (ok bool, holder string, err error) {
	if !validKey(key) || runID == "" || workflow == "" || lease <= 0 {
		return false, "", errors.New("cloud claims: invalid claim")
	}
	err = s.mutate(func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if err := lockClaims(ctx, tx, s.instanceID); err != nil {
			return err
		}
		if err := pruneHistory(ctx, tx, s.instanceID, epoch); err != nil {
			return err
		}
		for attempt := 0; attempt < 2; attempt++ {
			entry, token, rowEpoch, live, rowErr := s.readClaimForUpdate(ctx, tx, key)
			if errors.Is(rowErr, pgx.ErrNoRows) {
				entry, token, rowEpoch, live, rowErr = s.readClaimForUpdateBlocking(ctx, tx, key)
			}
			switch {
			case rowErr == nil:
				holder = entry.RunID
				if entry.ReleasePending {
					ok = false
					return nil
				}
				if entry.RunID == runID && live {
					if entry.Workflow != workflow {
						return ErrConflict
					}
					if rowEpoch != epoch {
						return ErrTransition
					}
					tag, err := tx.Exec(ctx, `
						UPDATE control.claim
						SET renewed_at=clock_timestamp(), expires_at=clock_timestamp()+$7
						WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
						  AND owner_token=$5 AND owner_epoch=$6`,
						s.instanceID, key.Gaggle, key.Provider, key.ExternalID,
						token, epoch, lease,
					)
					if err := changed(tag.RowsAffected(), err); err != nil {
						return err
					}
					if err := recordGrant(ctx, tx, s.instanceID, key, epoch); err != nil {
						return err
					}
					if err := recordFairnessState(ctx, tx, s.instanceID, key, runID, epoch); err != nil {
						return err
					}
					ok, holder = true, runID
					return nil
				}
				if live {
					ok = false
					return nil
				}
				tag, err := tx.Exec(ctx, `
					DELETE FROM control.claim
					WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
					  AND owner_token=$5 AND owner_epoch=$6 AND owner_epoch<=$7
					  AND expires_at<=clock_timestamp()`,
					s.instanceID, key.Gaggle, key.Provider, key.ExternalID,
					token, rowEpoch, epoch,
				)
				if err := changed(tag.RowsAffected(), err); err != nil {
					return err
				}
			case !errors.Is(rowErr, pgx.ErrNoRows):
				return rowErr
			}
			if _, err := tx.Exec(ctx, `
				DELETE FROM control.claim
				WHERE instance_id=$1 AND owner_epoch<=$2
				  AND expires_at<=clock_timestamp()`,
				s.instanceID, epoch,
			); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM control.claim
				WHERE instance_id=$1 AND owner_epoch<=$2
				  AND expires_at>clock_timestamp()`,
				s.instanceID, epoch,
			).Scan(&count); err != nil {
				return err
			}
			if count >= MaxClaims {
				return ErrCapacity
			}
			ownerToken, err := randomToken()
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO control.claim
					(instance_id, gaggle, provider, external_id, run_id, workflow,
					 owner_token, owner_epoch, claimed_at, renewed_at, expires_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp(),clock_timestamp(),clock_timestamp()+$9)
				ON CONFLICT (instance_id,gaggle,provider,external_id) DO NOTHING`,
				s.instanceID, key.Gaggle, key.Provider, key.ExternalID,
				runID, workflow, ownerToken, epoch, lease,
			)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			if err := recordGrant(ctx, tx, s.instanceID, key, epoch); err != nil {
				return err
			}
			if err := recordFairnessState(ctx, tx, s.instanceID, key, runID, epoch); err != nil {
				return err
			}
			ok, holder = true, runID
			return nil
		}
		return ErrTransition
	})
	return ok, holder, err
}

func recordFairnessState(
	ctx context.Context,
	tx pgx.Tx,
	instanceID string,
	key localscheduler.ClaimKey,
	runID string,
	epoch int64,
) error {
	stateKey := key.Gaggle + "|" + key.Provider + "|" + key.ExternalID
	tag, err := tx.Exec(ctx, `
		INSERT INTO control.scheduler_state
			(instance_id,scope,key,value,etag,owner_epoch,updated_at)
		VALUES ($1,'claim-fairness',$2,$3,1,$4,clock_timestamp())
		ON CONFLICT (instance_id,scope,key) DO UPDATE
		SET value=excluded.value,etag=control.scheduler_state.etag+1,
		    owner_epoch=excluded.owner_epoch,updated_at=clock_timestamp()
		WHERE control.scheduler_state.instance_id=$1
		  AND control.scheduler_state.owner_epoch<=$4`,
		instanceID, stateKey, []byte(runID), epoch,
	)
	return changed(tag.RowsAffected(), err)
}

// Release releases a legacy claim.
func (s *Store) Release(itemID, runID string, reason claimrelease.Reason) error {
	return s.ReleaseScoped(
		localscheduler.ClaimKey{Gaggle: "legacy", Provider: "legacy", ExternalID: itemID},
		runID, reason,
	)
}

// ReleaseScoped releases only a claim owned by this epoch and run.
func (s *Store) ReleaseScoped(key localscheduler.ClaimKey, runID string, reason claimrelease.Reason) error {
	if !validKey(key) || runID == "" {
		return errors.New("cloud claims: invalid release")
	}
	return s.mutate(func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if err := lockClaims(ctx, tx, s.instanceID); err != nil {
			return err
		}
		released, err := releaseToHistory(ctx, tx, s.instanceID, epoch, key, runID, "", false, false, reason)
		if err == nil && !released {
			return ErrTransition
		}
		return err
	})
}

// ReleaseEntry releases an entry using its persisted namespace.
func (s *Store) ReleaseEntry(entry localscheduler.ClaimEntry, runID string, reason claimrelease.Reason) error {
	if entry.Gaggle == "" || entry.Provider == "" {
		return s.Release(entry.ItemID, runID, reason)
	}
	return s.ReleaseScoped(localscheduler.ClaimKey{
		Gaggle: entry.Gaggle, Provider: entry.Provider, ExternalID: entry.ExternalID,
	}, runID, reason)
}

// RecordClaimVerification compare-and-swaps a provider observation.
func (s *Store) RecordClaimVerification(
	expected localscheduler.ClaimEntry,
	observation localscheduler.ClaimVerification,
) (updated bool, err error) {
	key := localscheduler.ClaimKey{
		Gaggle: expected.Gaggle, Provider: expected.Provider, ExternalID: expected.ExternalID,
	}
	if key.Gaggle == "" {
		key = localscheduler.ClaimKey{Gaggle: "legacy", Provider: "legacy", ExternalID: expected.ItemID}
	}
	err = s.mutate(func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if err := localscheduler.ValidateClaimVerification(expected, observation, now); err != nil {
			return err
		}
		current, token, rowEpoch, live, err := s.readClaimForUpdateBlocking(ctx, tx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.RunID != expected.RunID ||
			!current.ClaimedAt.Equal(expected.ClaimedAt) ||
			rowEpoch != epoch ||
			!live ||
			(!current.Verification.ObservedAt.IsZero() &&
				!observation.ObservedAt.After(current.Verification.ObservedAt)) {
			return nil
		}
		raw, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE control.claim SET verification=$8
			WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
			  AND run_id=$5 AND owner_token=$6 AND owner_epoch=$7
			  AND expires_at>clock_timestamp()`,
			s.instanceID, key.Gaggle, key.Provider, key.ExternalID,
			expected.RunID, token, epoch, raw,
		)
		updated = err == nil && tag.RowsAffected() == 1
		if !updated {
			return err
		}
		return recordGrant(ctx, tx, s.instanceID, key, epoch)
	})
	return updated, err
}

// AdoptClaims transfers live predecessor claims to the current fenced epoch.
// The predecessor epoch is an exact CAS input, so an adoption cannot steal a
// claim already adopted by another successor.
func (s *Store) AdoptClaims(predecessorEpoch int64) (int64, error) {
	if predecessorEpoch < 1 || predecessorEpoch >= s.handle.Epoch {
		return 0, errors.New("cloud claims: invalid predecessor epoch")
	}
	var adopted int64
	err := s.mutate(func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		if err := lockClaims(ctx, tx, s.instanceID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE control.claim SET owner_epoch=$3
			WHERE instance_id=$1 AND owner_epoch=$2
			  AND expires_at>clock_timestamp()`,
			s.instanceID, predecessorEpoch, epoch,
		)
		if err == nil {
			adopted = tag.RowsAffected()
		}
		return err
	})
	return adopted, err
}

// Snapshot returns active claims.
func (s *Store) Snapshot() []localscheduler.ClaimEntry {
	rows, err := s.pool.Query(context.Background(), claimSelect+`
		FROM control.claim WHERE instance_id=$1 ORDER BY gaggle,provider,external_id`,
		s.instanceID,
	)
	if err != nil {
		return nil
	}
	entries, err := pgx.CollectRows(rows, scanClaim)
	if err != nil {
		return nil
	}
	return entries
}

// ForRunAll returns all active claims owned by a run.
func (s *Store) ForRunAll(runID string) []localscheduler.ClaimEntry {
	entries := s.Snapshot()
	result := entries[:0]
	for _, entry := range entries {
		if entry.RunID == runID {
			result = append(result, entry)
		}
	}
	return result
}

// HistorySnapshot returns retained claim history, newest first, as the file
// ledger's HistorySnapshot does: one record per run and key, live or released.
func (s *Store) HistorySnapshot() []localscheduler.ClaimEntry {
	entries, err := queryHistory(context.Background(), s.pool, s.instanceID, `true`, historyNewestFirst)
	if err != nil {
		return nil
	}
	return entries
}

func (s *Store) mutate(action func(context.Context, pgx.Tx, int64) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	return s.manager.WithFence(ctx, s.handle, func(ctx context.Context, tx pgx.Tx) error {
		return action(ctx, tx, s.handle.Epoch)
	})
}

func lockClaims(ctx context.Context, tx pgx.Tx, instanceID string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('claims:' || $1, 0))", instanceID)
	return err
}

func (s *Store) readClaimForUpdate(
	ctx context.Context,
	tx pgx.Tx,
	key localscheduler.ClaimKey,
) (localscheduler.ClaimEntry, string, int64, bool, error) {
	return scanClaimWithOwnershipAndLive(tx.QueryRow(ctx, claimSelectWithOwnership+`,expires_at>clock_timestamp()`+`
		FROM control.claim
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
		FOR UPDATE SKIP LOCKED`,
		s.instanceID, key.Gaggle, key.Provider, key.ExternalID,
	))
}

func (s *Store) readClaimForUpdateBlocking(
	ctx context.Context,
	tx pgx.Tx,
	key localscheduler.ClaimKey,
) (localscheduler.ClaimEntry, string, int64, bool, error) {
	return scanClaimWithOwnershipAndLive(tx.QueryRow(ctx, claimSelectWithOwnership+`,expires_at>clock_timestamp()`+`
		FROM control.claim
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4
		FOR UPDATE`,
		s.instanceID, key.Gaggle, key.Provider, key.ExternalID,
	))
}

// claimSelect reads a claim, with release_pending_at as the ledger's
// ReleasePending (cleanup custody).
const claimSelect = `SELECT external_id,gaggle,provider,run_id,workflow,
	claimed_at,renewed_at,expires_at,verification,release_pending_at IS NOT NULL`
const claimSelectWithToken = claimSelect + `,owner_token`
const claimSelectWithOwnership = claimSelectWithToken + `,owner_epoch`

func scanClaim(row pgx.CollectableRow) (localscheduler.ClaimEntry, error) {
	return scanClaimRow(row)
}

func scanClaimRow(row interface{ Scan(...any) error }) (localscheduler.ClaimEntry, error) {
	var entry localscheduler.ClaimEntry
	var verification []byte
	err := row.Scan(
		&entry.ItemID, &entry.Gaggle, &entry.Provider, &entry.RunID, &entry.Workflow,
		&entry.ClaimedAt, &entry.RenewedAt, &entry.ExpiresAt, &verification, &entry.ReleasePending,
	)
	entry.ExternalID = entry.ItemID
	if len(verification) > 0 {
		err = errors.Join(err, json.Unmarshal(verification, &entry.Verification))
	}
	return entry, err
}

func scanClaimWithOwnershipAndLive(
	row interface{ Scan(...any) error },
) (localscheduler.ClaimEntry, string, int64, bool, error) {
	var entry localscheduler.ClaimEntry
	var verification []byte
	var token string
	var ownerEpoch int64
	var live bool
	err := row.Scan(
		&entry.ItemID, &entry.Gaggle, &entry.Provider, &entry.RunID, &entry.Workflow,
		&entry.ClaimedAt, &entry.RenewedAt, &entry.ExpiresAt, &verification, &entry.ReleasePending,
		&token, &ownerEpoch, &live,
	)
	entry.ExternalID = entry.ItemID
	if len(verification) > 0 {
		err = errors.Join(err, json.Unmarshal(verification, &entry.Verification))
	}
	return entry, token, ownerEpoch, live, err
}

// scanHistoryClaim reads one history record. A live or overwritten lease's
// record has no release time or reason yet.
func scanHistoryClaim(row pgx.CollectableRow) (localscheduler.ClaimEntry, error) {
	var entry localscheduler.ClaimEntry
	var verification, reason []byte
	err := row.Scan(
		&entry.ItemID, &entry.Gaggle, &entry.Provider, &entry.RunID, &entry.Workflow,
		&entry.ClaimedAt, &entry.RenewedAt, &entry.ExpiresAt, &verification,
		&entry.ReleasedAt, &reason, &entry.ReleasePending,
	)
	entry.ExternalID = entry.ItemID
	if len(verification) > 0 {
		err = errors.Join(err, json.Unmarshal(verification, &entry.Verification))
	}
	if len(reason) > 0 {
		err = errors.Join(err, json.Unmarshal(reason, &entry.ReleaseReason))
	}
	return entry, err
}

func validKey(key localscheduler.ClaimKey) bool {
	return strings.TrimSpace(key.Gaggle) != "" &&
		strings.TrimSpace(key.Provider) != "" &&
		strings.TrimSpace(key.ExternalID) != ""
}

func changed(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrTransition
	}
	return nil
}

func randomToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("cloud claims: owner token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

var _ localscheduler.ClaimStore = (*Store)(nil)
