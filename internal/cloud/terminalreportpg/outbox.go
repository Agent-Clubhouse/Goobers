package terminalreportpg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

// ErrDeliveryPending means another delivery of the key holds a live lease.
var ErrDeliveryPending = errors.New("terminal report delivery already in progress")

// Outbox runs terminalreport.Queue's publication protocol over the store for
// one acquired lease, so it satisfies terminalreport.Outbox. Log receives the
// same outcome annotations the local queue appends.
type Outbox struct {
	Store  Store
	Handle *lease.Handle
	Log    func(journal.Event) error
}

// Publish durably admits e, then attempts its delivery once. A key still
// deferred reports a provider rate-limit error, as the local queue does.
func (o Outbox) Publish(ctx context.Context, e terminalreport.Effect, apply func(context.Context, terminalreport.Effect) error) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := o.Store.Publish(persistCtx, o.Handle, e); err != nil {
		return err
	}
	key := e.Key()
	deliveries, err := o.Store.claim(ctx, o.Handle, 1, &key)
	if err != nil {
		return err
	}
	if len(deliveries) == 0 {
		return o.undeliverable(ctx, e)
	}
	return o.deliver(ctx, deliveries[0], apply)
}

// undeliverable explains why a just-admitted key could not be claimed.
func (o Outbox) undeliverable(ctx context.Context, e terminalreport.Effect) error {
	var deferred, leased bool
	var retryAt time.Time
	err := o.Store.Fence.WithFence(ctx, o.Handle, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT retry_at > clock_timestamp(), coalesce(lease_until > clock_timestamp(), false),
			       CASE WHEN retry_at = '-infinity' THEN 'epoch'::timestamptz ELSE retry_at END
			FROM control.terminal_report
			WHERE instance_id=$1 AND effect_key=$2 AND delivered_at IS NULL`,
			o.Handle.InstanceID, e.Key(),
		).Scan(&deferred, &leased, &retryAt)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return err
	case deferred:
		return &providers.RateLimitError{Provider: e.Repository.Provider, Reset: retryAt.UTC()}
	case leased:
		return ErrDeliveryPending
	default:
		return nil
	}
}

// Reconcile delivers at most budget due intents.
func (o Outbox) Reconcile(ctx context.Context, budget int, apply func(context.Context, terminalreport.Effect) error) error {
	deliveries, err := o.Store.Claim(ctx, o.Handle, budget)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range deliveries {
		if ctx.Err() != nil {
			// Unattempted leases expire on the database clock and are reclaimed.
			break
		}
		if err := o.deliver(ctx, d, apply); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (o Outbox) deliver(ctx context.Context, d Delivery, apply func(context.Context, terminalreport.Effect) error) error {
	attemptCtx, cancel := context.WithTimeout(ctx, d.CallTimeout)
	defer cancel()
	attemptCtx = gate.WithFailureReportBody(attemptCtx, d.Body, func(body string) error {
		return o.Store.Freeze(attemptCtx, o.Handle, d, body)
	})
	err := attemptCtx.Err()
	if err == nil {
		err = apply(attemptCtx, d.Effect)
	}
	return o.acknowledge(ctx, d, err)
}

// acknowledge settles d as the local queue does. A delivered, superseded or
// duplicate-cleanup outcome acknowledges d's generation. A failure defers it.
// When a newer intent was admitted meanwhile, or the outcome could not be
// journaled, the lease is only released, carrying forward a rate limit that
// still binds the target, so the newer intent is not delayed or charged an
// attempt.
func (o Outbox) acknowledge(ctx context.Context, d Delivery, deliveryErr error) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	outcome := "delivered"
	var duplicates *gate.DuplicateReportCleanupUnsupported
	switch {
	case errors.As(deliveryErr, &duplicates):
		outcome = "duplicate_cleanup_unsupported"
	case errors.Is(deliveryErr, terminalreport.ErrSuperseded):
		outcome = "superseded"
	case deliveryErr != nil:
		outcome = "deferred"
	}
	now := time.Now()
	delay := terminalreport.RetryDelay(deliveryErr, now)
	var carry time.Duration
	if _, limited := providers.AsRateLimitError(deliveryErr); limited {
		carry = delay
	}
	if outcome == "deferred" {
		d.Effect.Attempts++
		d.Effect.RetryAt = now.Add(delay)
	}
	logErr := o.record(d.Effect, outcome, duplicates)
	var storeErr error
	switch {
	case logErr != nil:
		storeErr = o.Store.Release(persistCtx, o.Handle, d, carry)
	case outcome == "deferred":
		_, storeErr = o.Store.Defer(persistCtx, o.Handle, d, delay)
		if errors.Is(storeErr, terminalreport.ErrSuperseded) {
			storeErr = o.Store.Release(persistCtx, o.Handle, d, carry)
		}
	default:
		storeErr = o.Store.Delivered(persistCtx, o.Handle, d)
	}
	// A newer generation or a reclaimed lease leaves the row to its next delivery.
	if errors.Is(storeErr, terminalreport.ErrSuperseded) || errors.Is(storeErr, ErrLeaseLost) {
		storeErr = nil
	}
	if errors.Is(deliveryErr, terminalreport.ErrSuperseded) || duplicates != nil {
		deliveryErr = nil
	}
	return errors.Join(deliveryErr, logErr, storeErr)
}

func (o Outbox) record(e terminalreport.Effect, outcome string, duplicates *gate.DuplicateReportCleanupUnsupported) error {
	if o.Log == nil {
		return nil
	}
	if err := o.Log(terminalreport.OutcomeEvent(e, outcome, duplicates)); err != nil {
		return fmt.Errorf("record terminal report %s: %w", outcome, err)
	}
	return nil
}

// Backlog is the undelivered terminal reports: how many, and how long the oldest
// has been waiting on the database clock. Zero means nothing is owed.
type Backlog struct {
	Pending int
	Oldest  time.Duration
}

// Backlog reads the outbox backlog for the handle's instance.
func (o Outbox) Backlog(ctx context.Context) (Backlog, error) {
	var backlog Backlog
	err := o.Store.Fence.WithFence(ctx, o.Handle, func(ctx context.Context, tx pgx.Tx) error {
		var oldest float64
		if err := tx.QueryRow(ctx, `
			SELECT count(*), coalesce(extract(epoch FROM clock_timestamp() - min(created_at)), 0)
			FROM control.terminal_report WHERE instance_id=$1 AND delivered_at IS NULL`,
			o.Handle.InstanceID).Scan(&backlog.Pending, &oldest); err != nil {
			return err
		}
		backlog.Oldest = time.Duration(oldest * float64(time.Second))
		return nil
	})
	return backlog, err
}
