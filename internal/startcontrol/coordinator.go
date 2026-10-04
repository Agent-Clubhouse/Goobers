// Package startcontrol binds shared queue controls to validated accepted source
// identities and exact archived configuration, independently of execution.
package startcontrol

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Archive resolves only the accepted generation and same-gaggle source identity.
// It must not select a replacement from the currently applied named catalog.
type Archive func(context.Context, Metadata) (*apiv1.Gaggle, error)

// Coordinator uses the shared store and an explicit host clock. Cursors are only
// bounded maintenance progress; they never own acceptance or execution custody.
type Coordinator struct {
	Cancellation func(context.Context, triggerqueue.StartControl) error
	Queue        *triggerqueue.Store
	Archive      Archive
	Now          func() time.Time
	mu           sync.Mutex
	cursor       string
}

// Ensure derives immutable controls once. Exact replay reads the pinned control
// even after the source or original accepted configuration ceases to be current.
func (c *Coordinator) Ensure(ctx context.Context, record triggerqueue.Record) (triggerqueue.StartControl, error) {
	prior, err := c.Queue.PinnedStartControl(ctx, record.ID)
	if err == nil {
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return prior, err
	}
	meta, err := Describe(ctx, c.Queue, record)
	if err != nil {
		return prior, err
	}
	if c.Archive == nil {
		return prior, errors.New("start controls: archived policy unavailable")
	}
	gaggle, err := c.Archive(ctx, meta)
	if err != nil {
		return prior, err
	}
	if gaggle == nil || gaggle.Name != meta.Scope.Gaggle {
		return prior, triggerqueue.ErrConflict
	}
	meta.Scope.Deadline, err = deadline(gaggle.Spec.StartQueue, meta.Scope.Source, record.AcceptedAt, meta.ExplicitDeadline)
	if err != nil {
		return prior, err
	}
	return c.Queue.PinStartControl(ctx, record.ID, meta.Scope)
}

// BeforeDispatch reconciles only proven unattempted custody. A false result
// means the retained receipt is terminal; it is never permission for a new ID.
func (c *Coordinator) BeforeDispatch(ctx context.Context, record triggerqueue.Record) (bool, error) {
	control, err := c.Ensure(ctx, record)
	if errors.Is(err, ErrLegacyUnscoped) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	control, _, err = c.Queue.ReconcileStartCancellation(ctx, control.Scope.Gaggle, record.ID, c.Now())
	if err != nil {
		return false, err
	}
	control, _, err = c.Queue.ExpireStartControl(ctx, control.Scope.Gaggle, record.ID, c.Now())
	return control.Record.State != triggerqueue.Rejected, err
}

// Sweep bounds inventory and maintenance to one page. Unclassifiable legacy
// receipts stay retained and never borrow authority from a current name lookup.
func (c *Coordinator) Sweep(ctx context.Context) error {
	if !c.mu.TryLock() {
		return nil
	}
	defer c.mu.Unlock()
	records, err := c.Queue.StartControlInventory(ctx, c.cursor, 50)
	if err != nil {
		return err
	}
	var failures []error
	for _, record := range records {
		if err = ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		_, err = c.BeforeDispatch(ctx, record)
		if err != nil && !errors.Is(err, ErrLegacyUnscoped) && !errors.Is(err, triggerqueue.ErrTypedStartSettlement) {
			failures = append(failures, err)
		}
		if c.Cancellation != nil {
			if control, readErr := c.Queue.PinnedStartControl(ctx, record.ID); readErr == nil && control.Cancellation != nil && control.Disposition == "" && control.CancellationOutcome == "" {
				if stopErr := c.Cancellation(ctx, control); stopErr != nil {
					failures = append(failures, stopErr)
				}
			}
		}
		c.cursor = record.ID
	}
	if len(records) < 50 {
		c.cursor = ""
	}
	return errors.Join(failures...)
}

func deadline(policy *apiv1.StartQueuePolicy, source string, accepted, explicit time.Time) (time.Time, error) {
	if source == "child" {
		return time.Time{}, nil
	}
	seconds := int32(604800)
	if source == "schedule" {
		seconds = 3600
	}
	if policy != nil {
		value := policy.PendingDeadlineSeconds
		if source == "schedule" {
			value = policy.ScheduledDeadlineSeconds
		}
		if value != nil {
			seconds = *value
		}
	}
	if seconds < 1 || seconds > 7776000 {
		return time.Time{}, errors.New("start controls: invalid captured deadline")
	}
	until := accepted.Add(time.Duration(seconds) * time.Second)
	if !explicit.IsZero() && explicit.Before(until) {
		until = explicit
	}
	// A legacy explicit deadline may already precede durable transfer; immediate
	// expiry is represented at acceptance rather than widening its wait.
	if until.Before(accepted) {
		until = accepted
	}
	return until.UTC(), nil
}
