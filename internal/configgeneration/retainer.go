package configgeneration

import (
	"context"
	"errors"
	"sync"

	"github.com/goobers/goobers/internal/platform/lock"
)

// Retainer owns the generations admitted by one process. A process pin lives
// until shutdown, including the interval before a starter writes its journal.
// DurablePins must return every retained journal pin, including paused and
// recoverable runs. Store's finite capacity bounds the process pin set as well
// as disk use; exhausting it refuses a new generation without evicting an owner.
type Retainer struct {
	Store       Store
	DurablePins func(context.Context) (map[string]bool, error)
	mu          sync.Mutex
	pins        map[string]*lock.Handle
}

// Keep retains a validated generation before publishing its starter. The caller
// must share this Retainer across initial composition and configuration reloads.
func (r *Retainer) Keep(ctx context.Context, data []byte, digest string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	protected := make(map[string]bool, len(r.pins))
	if r.DurablePins != nil {
		durable, err := r.DurablePins(ctx)
		if err != nil {
			return "", err
		}
		for pin, retained := range durable {
			protected[pin] = retained
		}
	}
	for pin := range r.pins {
		protected[pin] = true
	}
	if r.pins[digest] != nil {
		return r.Store.Keep(ctx, data, digest, protected)
	}
	store := r.Store
	store.DurablePins = r.DurablePins
	path, lease, err := store.KeepAndAcquire(ctx, data, digest, protected)
	if err != nil {
		return "", err
	}
	if r.pins == nil {
		r.pins = make(map[string]*lock.Handle)
	}
	r.pins[digest] = lease
	return path, nil
}

// Close releases process ownership after its starters and runs have stopped.
func (r *Retainer) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for digest, lease := range r.pins {
		errs = append(errs, lease.Release())
		delete(r.pins, digest)
	}
	return errors.Join(errs...)
}
