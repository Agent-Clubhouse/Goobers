package claimsclient

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrSharedExecutionExpired denies execution after shared authority has ended.
var ErrSharedExecutionExpired = errors.New("shared claim execution authority expired")

// StartExecutionFence monitors persisted, provider-acknowledged deadlines for
// one run. The snapshot reader must honor cancellation. It runs independently
// of the deadline timer: a stalled read cannot extend known authority. Callers
// must stop the monitor when execution finishes and propagate context.Cause
// when it cancels their executor. This does not acquire or renew any lease.
func StartExecutionFence(parent context.Context, runID string, snapshot func(context.Context) (Listing, error)) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithCancelCause(parent)
	stop := func() { cancel(context.Canceled) }
	initial, err := snapshot(ctx)
	state := executionClaims{runID: runID, held: make(map[Key]Entry)}
	if err == nil {
		err = state.update(initial, time.Now())
	}
	if err != nil {
		cancel(err)
		return ctx, stop, err
	}
	type observation struct {
		listing Listing
		err     error
	}
	updates := make(chan observation)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				listing, err := snapshot(ctx)
				select {
				case updates <- observation{listing, err}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() {
		for {
			var expiry <-chan time.Time
			var timer *time.Timer
			if deadline := state.deadline(); !deadline.IsZero() {
				timer = time.NewTimer(time.Until(deadline))
				expiry = timer.C
			}
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-expiry:
				cancel(ErrSharedExecutionExpired)
				return
			case update := <-updates:
				if timer != nil {
					timer.Stop()
				}
				err := update.err
				if err == nil {
					err = state.update(update.listing, time.Now())
				}
				if err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return ctx, stop, nil
}

type executionClaims struct {
	runID string
	held  map[Key]Entry
}

func (s *executionClaims) deadline() time.Time {
	var deadline time.Time
	for _, entry := range s.held {
		if deadline.IsZero() || entry.SharedDeadline.Before(deadline) {
			deadline = entry.SharedDeadline
		}
	}
	return deadline
}

func (s *executionClaims) update(listing Listing, now time.Time) error {
	if deadline := s.deadline(); !deadline.IsZero() && !deadline.After(now) {
		return ErrSharedExecutionExpired
	}
	active := make(map[Key]bool)
	for _, entry := range listing.Entries {
		if entry.RunID != s.runID || entry.SharedDeadline.IsZero() {
			continue
		}
		if !entry.SharedDeadline.After(now) || !entry.ExpiresAt.After(now) {
			return ErrSharedExecutionExpired
		}
		key := KeyForEntry(entry)
		active[key] = true
		if previous, ok := s.held[key]; ok && previous.SharedOwner != entry.SharedOwner {
			return fmt.Errorf("shared claim execution incarnation changed")
		}
		if entry.ExpiresAt.Before(entry.SharedDeadline) {
			entry.SharedDeadline = entry.ExpiresAt
		}
		s.held[key] = entry
	}
	for _, entry := range listing.History {
		if entry.RunID != s.runID || entry.SharedDeadline.IsZero() || entry.ReleasedAt == nil {
			continue
		}
		if !entry.ReleasedAt.Before(entry.SharedDeadline) {
			return ErrSharedExecutionExpired
		}
		key := KeyForEntry(entry)
		if active[key] {
			continue
		}
		if held, ok := s.held[key]; ok && held.SharedOwner == entry.SharedOwner && !entry.ReleasedAt.Before(held.ClaimedAt) {
			delete(s.held, key)
		}
	}
	// Missing entries alone are not proof of a valid early release. Keep
	// their timers, including when a reaper or truncated read omits them.
	return nil
}
