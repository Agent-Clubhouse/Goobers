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
	"github.com/jackc/pgx/v5/pgtype"
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

// Publish admits the newest intent for its key. Re-publishing an identical
// pending intent resolves to its stored generation before any capacity check.
// A different intent, or any intent for an already delivered key, advances the
// generation, so it is delivered again and an in-flight older delivery can no
// longer acknowledge it.
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

// resolve reports whether the key exists, leaving an identical pending intent
// at its stored generation and advancing anything else.
func (s Store) resolve(ctx context.Context, tx pgx.Tx, handle *lease.Handle, a admission, generation *int64) (bool, error) {
	var stored []byte
	var storedIdentity string
	var delivered bool
	err := tx.QueryRow(ctx, `
		SELECT effect, report_identity, generation, delivered_at IS NOT NULL FROM control.terminal_report
		WHERE instance_id=$1 AND effect_key=$2 FOR UPDATE`,
		handle.InstanceID, a.key,
	).Scan(&stored, &storedIdentity, generation, &delivered)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read terminal report: %w", err)
	case !delivered && bytes.Equal(stored, a.encoded):
		return true, nil
	default:
		// A frozen body belongs to its pending report identity; a delivered
		// report or a new identity renders afresh.
		return true, s.supersede(ctx, tx, handle, a, !delivered && a.identity == storedIdentity, generation)
	}
}

func (s Store) supersede(ctx context.Context, tx pgx.Tx, handle *lease.Handle, a admission, keepBody bool, generation *int64) error {
	err := tx.QueryRow(ctx, `
		UPDATE control.terminal_report
		SET effect=$3, report_identity=$4, generation=nextval('control.terminal_report_generation'), delivered_at=NULL,
		    body=CASE WHEN $5 THEN body END,
		    retry_at=CASE WHEN retry_at > now() THEN retry_at ELSE '-infinity' END,
		    attempts=CASE WHEN retry_at > now() THEN attempts ELSE 0 END,
		    owner_epoch=$6
		WHERE instance_id=$1 AND effect_key=$2 AND owner_epoch <= $6
		RETURNING generation`,
		handle.InstanceID, a.key, a.encoded, a.identity, keepBody, handle.Epoch,
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
	if err := tx.QueryRow(ctx, `
		INSERT INTO control.terminal_report
			(instance_id, effect_key, effect, report_identity, generation, owner_epoch)
		VALUES ($1, $2, $3, $4, nextval('control.terminal_report_generation'), $5)
		RETURNING generation`,
		handle.InstanceID, a.key, a.encoded, a.identity, handle.Epoch,
	).Scan(generation); err != nil {
		return fmt.Errorf("admit terminal report: %w", err)
	}
	return nil
}

// Claim leases up to limit due intents. SKIP LOCKED keeps concurrent claimers
// disjoint, and an unexpired lease on the database clock is never re-issued.
func (s Store) Claim(ctx context.Context, handle *lease.Handle, limit int) ([]Delivery, error) {
	return s.claim(ctx, handle, limit, nil)
}

func (s Store) claim(ctx context.Context, handle *lease.Handle, limit int, key *string) ([]Delivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	var deliveries []Delivery
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH due AS MATERIALIZED (
				SELECT effect_key FROM control.terminal_report
				WHERE instance_id=$1 AND delivered_at IS NULL
				  AND retry_at <= clock_timestamp()
				  AND (lease_until IS NULL OR lease_until <= clock_timestamp())
				  AND owner_epoch <= $2
				  AND ($5::text IS NULL OR effect_key=$5)
				ORDER BY retry_at, effect_key
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			)
			UPDATE control.terminal_report AS t
			SET lease_token=gen_random_uuid(), lease_until=clock_timestamp()+$4::interval, owner_epoch=$2
			FROM due
			WHERE t.instance_id=$1 AND t.effect_key=due.effect_key
			RETURNING t.effect_key, t.generation, t.lease_token::text, t.effect, coalesce(t.body, ''), t.attempts, t.retry_at`,
			handle.InstanceID, handle.Epoch, limit, s.leaseDuration(), key,
		)
		if err != nil {
			return fmt.Errorf("claim terminal reports: %w", err)
		}
		deliveries, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Delivery, error) {
			var d Delivery
			var encoded []byte
			var retryAt pgtype.Timestamptz
			if err := row.Scan(&d.Key, &d.Generation, &d.LeaseToken, &encoded, &d.Body, &d.Attempts, &retryAt); err != nil {
				return d, err
			}
			d.OwnerEpoch, d.CallTimeout = handle.Epoch, s.callTimeout()
			d.IdempotencyKey = fmt.Sprintf("%s/%d", d.Key, d.Generation)
			if err := json.Unmarshal(encoded, &d.Effect); err != nil {
				return d, err
			}
			d.Effect.Attempts = d.Attempts
			if retryAt.InfinityModifier == pgtype.Finite {
				d.Effect.RetryAt = retryAt.Time.UTC()
			}
			return d, nil
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
	superseded := false
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
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
			// Commit the release; returning an error here would roll it back.
			superseded = true
			return s.releaseLease(ctx, tx, handle, d)
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
	if err == nil && superseded {
		return terminalreport.ErrSuperseded
	}
	return err
}

func (s Store) releaseLease(ctx context.Context, tx pgx.Tx, handle *lease.Handle, d Delivery) error {
	tag, err := tx.Exec(ctx, `
		UPDATE control.terminal_report SET lease_token=NULL, lease_until=NULL
		WHERE instance_id=$1 AND effect_key=$2 AND lease_token=$3::uuid AND owner_epoch=$4`,
		handle.InstanceID, d.Key, d.LeaseToken, handle.Epoch,
	)
	if err != nil {
		return fmt.Errorf("release terminal report lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Defer releases d's lease and postpones the key by delay on the database
// clock, counting the attempt. A later retry time already recorded for the
// key is never shortened. It applies only to d's own generation: when a newer
// intent was admitted meanwhile it changes nothing and reports
// terminalreport.ErrSuperseded, so the newer intent is not delayed. It returns
// the recorded retry time.
func (s Store) Defer(ctx context.Context, handle *lease.Handle, d Delivery, delay time.Duration) (time.Time, error) {
	var retryAt time.Time
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE control.terminal_report
			SET retry_at=GREATEST(retry_at, clock_timestamp()+$5::interval), attempts=attempts+1,
			    lease_token=NULL, lease_until=NULL
			WHERE instance_id=$1 AND effect_key=$2 AND lease_token=$3::uuid
			  AND owner_epoch=$4 AND generation=$6 AND delivered_at IS NULL
			RETURNING retry_at`,
			handle.InstanceID, d.Key, d.LeaseToken, handle.Epoch, max(delay, 0), d.Generation,
		).Scan(&retryAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return s.leaseOutcome(ctx, tx, handle, d)
		}
		if err != nil {
			return fmt.Errorf("defer terminal report: %w", err)
		}
		return nil
	})
	return retryAt.UTC(), err
}

// leaseOutcome distinguishes a superseded generation, whose lease d still
// holds, from a lost lease.
func (s Store) leaseOutcome(ctx context.Context, tx pgx.Tx, handle *lease.Handle, d Delivery) error {
	var held bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM control.terminal_report
			WHERE instance_id=$1 AND effect_key=$2 AND lease_token=$3::uuid
			  AND owner_epoch=$4 AND delivered_at IS NULL)`,
		handle.InstanceID, d.Key, d.LeaseToken, handle.Epoch,
	).Scan(&held); err != nil {
		return fmt.Errorf("read terminal report lease: %w", err)
	}
	if held {
		return terminalreport.ErrSuperseded
	}
	return ErrLeaseLost
}

// Release gives up d's lease, so the key is claimed again. It does not count
// an attempt. A positive notBefore (a provider rate limit that still binds the
// key's target) keeps the key undue for at least that long; a later recorded
// retry time is never shortened. It reports ErrLeaseLost if the lease was
// already reclaimed.
func (s Store) Release(ctx context.Context, handle *lease.Handle, d Delivery, notBefore time.Duration) error {
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		if notBefore > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE control.terminal_report
				SET retry_at=GREATEST(retry_at, clock_timestamp()+$5::interval)
				WHERE instance_id=$1 AND effect_key=$2 AND lease_token=$3::uuid AND owner_epoch=$4`,
				handle.InstanceID, d.Key, d.LeaseToken, handle.Epoch, notBefore,
			); err != nil {
				return fmt.Errorf("carry terminal report rate limit: %w", err)
			}
		}
		return s.releaseLease(ctx, tx, handle, d)
	})
}

// AdoptAll moves the undelivered rows of EVERY older epoch to the live epoch in
// one fenced statement and voids their delivery leases, so no predecessor can
// acknowledge them. Adopt only takes the exact predecessor; after two or more
// takeovers between uses (a successor that crashed before adopting) the older
// epochs' rows would otherwise be stranded. It is the only adoption: an
// exact-predecessor variant would strand rows after a skipped epoch.
func (s Store) AdoptAll(ctx context.Context, handle *lease.Handle) (int64, error) {
	var adopted int64
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.terminal_report
			SET owner_epoch=$2, lease_token=NULL, lease_until=NULL
			WHERE instance_id=$1 AND owner_epoch < $2 AND delivered_at IS NULL`,
			handle.InstanceID, handle.Epoch,
		)
		if err != nil {
			return fmt.Errorf("adopt all terminal reports: %w", err)
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
