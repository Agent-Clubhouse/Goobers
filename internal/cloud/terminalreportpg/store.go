// Package terminalreportpg stores the terminal-report outbox in PostgreSQL.
// Delivery is at-least-once: a claim leases one generation on the database
// clock, and only the live owner holding that lease token can acknowledge it.
package terminalreportpg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/terminalreport"
)

const (
	// DefaultCallTimeout bounds one provider call made under a delivery lease.
	DefaultCallTimeout = 30 * time.Second
	// DefaultSkewMargin separates the caller's deadline from lease expiry.
	DefaultSkewMargin = 30 * time.Second
	// DefaultMaxPending bounds undelivered intents per instance.
	DefaultMaxPending = 10000
	// DefaultRetention keeps delivered rows as idempotency evidence.
	DefaultRetention = 7 * 24 * time.Hour
)

var (
	// ErrFull means the pending outbox is at capacity.
	ErrFull = errors.New("terminal report: outbox capacity exhausted")
	// ErrLeaseLost means the delivery lease was reclaimed or adopted.
	ErrLeaseLost = errors.New("terminal report: delivery lease lost")
)

// Store is the PostgreSQL terminal-report outbox for one lease.
type Store struct {
	Pool        *pgxpool.Pool
	Fence       *lease.Manager
	CallTimeout time.Duration
	SkewMargin  time.Duration
	MaxPending  int
	Retention   time.Duration
}

// Delivery is one leased generation. CallTimeout is a duration, not a wall
// time: callers bound the provider call with their own monotonic clock.
type Delivery struct {
	Key            string
	Generation     int64
	LeaseToken     string
	OwnerEpoch     int64
	Effect         terminalreport.Effect
	Body           string
	Attempts       int
	CallTimeout    time.Duration
	IdempotencyKey string
}

func (s Store) callTimeout() time.Duration {
	if s.CallTimeout > 0 {
		return s.CallTimeout
	}
	return DefaultCallTimeout
}

func (s Store) leaseDuration() time.Duration {
	margin := s.SkewMargin
	if margin <= 0 {
		margin = DefaultSkewMargin
	}
	return s.callTimeout() + margin
}

func (s Store) maxPending() int {
	if s.MaxPending > 0 {
		return s.MaxPending
	}
	return DefaultMaxPending
}

func (s Store) retention() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return DefaultRetention
}

func encodeEffect(e terminalreport.Effect) ([]byte, error) {
	e.RetryAt, e.Attempts = time.Time{}, 0
	return json.Marshal(e)
}

// Publish admits the newest intent for its key. Re-publishing the identical
// intent resolves to the stored generation before any capacity check; a
// different intent advances the generation so an in-flight older delivery
// can no longer acknowledge it.
func (s Store) Publish(ctx context.Context, handle *lease.Handle, effect terminalreport.Effect) (int64, error) {
	encoded, err := encodeEffect(effect)
	if err != nil {
		return 0, err
	}
	admission := admission{key: effect.Key(), identity: effect.Identity(), encoded: encoded}
	var generation int64
	err = s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		found, err := s.resolve(ctx, tx, handle, admission, &generation)
		if found || err != nil {
			return err
		}
		// New keys serialize on the instance's capacity lock, then re-resolve so
		// a concurrent admission of the same key is an existing key, not a race.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('control.terminal_report:' || $1, 0))", handle.InstanceID); err != nil {
			return fmt.Errorf("lock terminal report capacity: %w", err)
		}
		if found, err = s.resolve(ctx, tx, handle, admission, &generation); found || err != nil {
			return err
		}
		return s.admit(ctx, tx, handle, admission, &generation)
	})
	return generation, err
}

type admission struct {
	key, identity string
	encoded       []byte
}

// resolve reports whether the key exists, leaving an identical intent at its
// stored generation and advancing a different one.
func (s Store) resolve(ctx context.Context, tx pgx.Tx, handle *lease.Handle, a admission, generation *int64) (bool, error) {
	var stored []byte
	var storedIdentity string
	err := tx.QueryRow(ctx, `
		SELECT effect, report_identity, generation FROM control.terminal_report
		WHERE instance_id=$1 AND effect_key=$2 FOR UPDATE`,
		handle.InstanceID, a.key,
	).Scan(&stored, &storedIdentity, generation)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read terminal report: %w", err)
	case bytes.Equal(stored, a.encoded):
		return true, nil
	default:
		return true, s.supersede(ctx, tx, handle, a, storedIdentity, generation)
	}
}

func (s Store) supersede(ctx context.Context, tx pgx.Tx, handle *lease.Handle, a admission, storedIdentity string, generation *int64) error {
	// A frozen body belongs to its report identity; a new identity re-renders.
	err := tx.QueryRow(ctx, `
		UPDATE control.terminal_report
		SET effect=$3, report_identity=$4, generation=generation+1, delivered_at=NULL,
		    body=CASE WHEN $5 THEN body END,
		    retry_at=CASE WHEN retry_at > now() THEN retry_at ELSE '-infinity' END,
		    attempts=CASE WHEN retry_at > now() THEN attempts ELSE 0 END,
		    owner_epoch=$6
		WHERE instance_id=$1 AND effect_key=$2 AND owner_epoch <= $6
		RETURNING generation`,
		handle.InstanceID, a.key, a.encoded, a.identity, a.identity == storedIdentity, handle.Epoch,
	).Scan(generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return lease.ErrStale
	}
	if err != nil {
		return fmt.Errorf("supersede terminal report: %w", err)
	}
	return nil
}

func (s Store) admit(ctx context.Context, tx pgx.Tx, handle *lease.Handle, a admission, generation *int64) error {
	var pending int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM control.terminal_report
		WHERE instance_id=$1 AND delivered_at IS NULL`,
		handle.InstanceID,
	).Scan(&pending); err != nil {
		return fmt.Errorf("count terminal reports: %w", err)
	}
	if pending >= s.maxPending() {
		return ErrFull
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO control.terminal_report
			(instance_id, effect_key, effect, report_identity, generation, owner_epoch)
		VALUES ($1, $2, $3, $4, 1, $5)
`,
		handle.InstanceID, a.key, a.encoded, a.identity, handle.Epoch,
	)
	if err != nil {
		return fmt.Errorf("admit terminal report: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("admit terminal report: inserted %d rows", tag.RowsAffected())
	}
	*generation = 1
	return nil
}

// Claim leases up to limit due intents. SKIP LOCKED keeps concurrent claimers
// disjoint, and an unexpired lease on the database clock is never re-issued.
func (s Store) Claim(ctx context.Context, handle *lease.Handle, limit int) ([]Delivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	var deliveries []Delivery
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH due AS (
				SELECT effect_key FROM control.terminal_report
				WHERE instance_id=$1 AND delivered_at IS NULL
				  AND retry_at <= clock_timestamp()
				  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
				  AND owner_epoch <= $2
				ORDER BY retry_at, effect_key
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			)
			UPDATE control.terminal_report AS t
			SET lease_token=gen_random_uuid(), lease_until=clock_timestamp()+$4::interval, owner_epoch=$2
			FROM due
			WHERE t.instance_id=$1 AND t.effect_key=due.effect_key
			RETURNING t.effect_key, t.generation, t.lease_token::text, t.effect, coalesce(t.body, ''), t.attempts`,
			handle.InstanceID, handle.Epoch, limit, s.leaseDuration(),
		)
		if err != nil {
			return fmt.Errorf("claim terminal reports: %w", err)
		}
		deliveries, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Delivery, error) {
			var d Delivery
			var encoded []byte
			if err := row.Scan(&d.Key, &d.Generation, &d.LeaseToken, &encoded, &d.Body, &d.Attempts); err != nil {
				return d, err
			}
			d.OwnerEpoch, d.CallTimeout = handle.Epoch, s.callTimeout()
			d.IdempotencyKey = fmt.Sprintf("%s/%d", d.Key, d.Generation)
			return d, json.Unmarshal(encoded, &d.Effect)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return deliveries, nil
}

// Freeze persists the rendered body before any provider write. A newer
// generation or a lost lease refuses the write with ErrSuperseded.
func (s Store) Freeze(ctx context.Context, handle *lease.Handle, d Delivery, body string) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.terminal_report SET body=$6
			WHERE instance_id=$1 AND effect_key=$2 AND generation=$3
			  AND lease_token=$4::uuid AND owner_epoch=$5 AND delivered_at IS NULL`,
			handle.InstanceID, d.Key, d.Generation, d.LeaseToken, handle.Epoch, body,
		)
		if err != nil {
			return fmt.Errorf("freeze terminal report body: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return terminalreport.ErrSuperseded
		}
		return nil
	})
}

// Delivered acknowledges d. A newer admitted generation releases the lease
// and reports ErrSuperseded so that generation is delivered too; a reclaimed
// or adopted lease reports ErrLeaseLost and changes nothing.
func (s Store) Delivered(ctx context.Context, handle *lease.Handle, d Delivery) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		var generation int64
		err := tx.QueryRow(ctx, `
			SELECT generation FROM control.terminal_report
			WHERE instance_id=$1 AND effect_key=$2 AND lease_token=$3::uuid
			  AND owner_epoch=$4 AND delivered_at IS NULL
			FOR UPDATE`,
			handle.InstanceID, d.Key, d.LeaseToken, handle.Epoch,
		).Scan(&generation)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("read terminal report lease: %w", err)
		}
		if generation != d.Generation {
			if err := s.releaseLease(ctx, tx, handle, d); err != nil {
				return err
			}
			return terminalreport.ErrSuperseded
		}
		_, err = tx.Exec(ctx, `
			UPDATE control.terminal_report
			SET delivered_at=now(), delivered_generation=generation, lease_token=NULL, lease_until=NULL
			WHERE instance_id=$1 AND effect_key=$2 AND lease_token=$3::uuid AND owner_epoch=$4`,
			handle.InstanceID, d.Key, d.LeaseToken, handle.Epoch,
		)
		if err != nil {
			return fmt.Errorf("acknowledge terminal report: %w", err)
		}
		return nil
	})
}

func (s Store) releaseLease(ctx context.Context, tx pgx.Tx, handle *lease.Handle, d Delivery) error {
	if _, err := tx.Exec(ctx, `
		UPDATE control.terminal_report SET lease_token=NULL, lease_until=NULL
		WHERE instance_id=$1 AND effect_key=$2 AND lease_token=$3::uuid AND owner_epoch=$4`,
		handle.InstanceID, d.Key, d.LeaseToken, handle.Epoch,
	); err != nil {
		return fmt.Errorf("release terminal report lease: %w", err)
	}
	return nil
}

// Defer releases d's lease and postpones the key by delay on the database
// clock. A later retry time already recorded for the key is never shortened.
func (s Store) Defer(ctx context.Context, handle *lease.Handle, d Delivery, delay time.Duration) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.terminal_report
			SET retry_at=GREATEST(retry_at, clock_timestamp()+$5::interval), attempts=attempts+1,
			    lease_token=NULL, lease_until=NULL
			WHERE instance_id=$1 AND effect_key=$2 AND lease_token=$3::uuid
			  AND owner_epoch=$4 AND delivered_at IS NULL`,
			handle.InstanceID, d.Key, d.LeaseToken, handle.Epoch, max(delay, 0),
		)
		if err != nil {
			return fmt.Errorf("defer terminal report: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}

// Adopt moves a predecessor's undelivered rows to the live epoch and voids
// their delivery leases, so the predecessor can no longer acknowledge them.
// The predecessor epoch must match exactly and be older than the live one.
func (s Store) Adopt(ctx context.Context, handle *lease.Handle, predecessorEpoch int64) (int64, error) {
	var adopted int64
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.terminal_report
			SET owner_epoch=$2, lease_token=NULL, lease_until=NULL
			WHERE instance_id=$1 AND owner_epoch=$3 AND $3 < $2 AND delivered_at IS NULL`,
			handle.InstanceID, handle.Epoch, predecessorEpoch,
		)
		if err != nil {
			return fmt.Errorf("adopt terminal reports: %w", err)
		}
		adopted = tag.RowsAffected()
		return nil
	})
	return adopted, err
}

// Prune deletes delivered rows older than the retention on the database clock.
func (s Store) Prune(ctx context.Context, handle *lease.Handle) (int64, error) {
	var pruned int64
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM control.terminal_report
			WHERE instance_id=$1 AND delivered_at IS NOT NULL
			  AND delivered_at < now() - $2::interval AND owner_epoch <= $3`,
			handle.InstanceID, s.retention(), handle.Epoch,
		)
		if err != nil {
			return fmt.Errorf("prune terminal reports: %w", err)
		}
		pruned = tag.RowsAffected()
		return nil
	})
	return pruned, err
}
