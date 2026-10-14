package runner

import (
	"context"
	"sync"
)

// A live branch owns one slot only while it can execute stages. Durable child
// waiting may return that slot while its branch goroutine remains alive. The
// root dispatcher observes releases without treating the branch as finished.
// All state is scoped to one bounded parallel block; nothing is persisted here.
type parallelBranchSlots struct {
	occupied chan struct{}
	changed  chan struct{}
}

type parallelBranchSlot struct {
	pool *parallelBranchSlots
	mu   sync.Mutex
	held bool
}

func newParallelBranchSlots(limit int) *parallelBranchSlots {
	return &parallelBranchSlots{occupied: make(chan struct{}, limit), changed: make(chan struct{}, 1)}
}

func (p *parallelBranchSlots) tryAcquire() (*parallelBranchSlot, bool) {
	select {
	case p.occupied <- struct{}{}:
		return &parallelBranchSlot{pool: p, held: true}, true
	default:
		return nil, false
	}
}

func (s *parallelBranchSlot) acquire(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.held {
		return nil
	}
	select {
	case s.pool.occupied <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.pool.release()
			return err
		}
		s.held = true
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *parallelBranchSlot) release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held {
		return
	}
	s.held = false
	s.pool.release()
}

func (p *parallelBranchSlots) release() {
	<-p.occupied
	select {
	case p.changed <- struct{}{}:
	default:
	}
}
