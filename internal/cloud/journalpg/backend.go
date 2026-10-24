// Package journalpg implements the shared journal contracts using PostgreSQL.
// All mutations are bounded transactions fenced by both the controller lease
// epoch and an opaque per-open journal owner token.
package journalpg

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

// Config binds one instance to the live controller lease. Reader methods need
// only Pool and InstanceID; mutation methods refuse missing lease ownership.
type Config struct {
	Pool       *pgxpool.Pool
	InstanceID string
	Lease      *lease.Manager
	LiveOwner  func() *lease.Handle
	// InstanceClock timestamps instance-log events before partition routing.
	// Nil uses time.Now.
	InstanceClock func() time.Time
}

// Backend is safe for concurrent use. Each recovered writer gets a new token;
// old process handles cannot acquire new authority through a lease refresh.
type Backend struct {
	config    Config
	mu        sync.Mutex
	runs      map[journal.RunHandle]*runOwner
	instances map[journal.InstanceHandle]*instanceOwner
	dirty     chan struct{}
}

type runOwner struct {
	key     journal.RunKey
	token   journal.OwnerToken
	epoch   int64
	options journal.RunWriterOptions
}

type instanceOwner struct {
	token  journal.OwnerToken
	epoch  int64
	stream string
}

// New constructs a writer/reader backend without acquiring journal ownership.
func New(config Config) (*Backend, error) {
	if config.Pool == nil || config.InstanceID == "" {
		return nil, errors.New("journalpg: pool and instance ID are required")
	}
	if config.InstanceClock == nil {
		config.InstanceClock = time.Now
	}
	return &Backend{config: config, runs: map[journal.RunHandle]*runOwner{}, instances: map[journal.InstanceHandle]*instanceOwner{}, dirty: make(chan struct{}, 1)}, nil
}

func (b *Backend) liveOwner() (*lease.Handle, error) {
	if b.config.Lease == nil || b.config.LiveOwner == nil {
		return nil, errors.New("journalpg: live lease required for mutation")
	}
	h := b.config.LiveOwner()
	if h == nil || h.InstanceID != b.config.InstanceID {
		return nil, lease.ErrStale
	}
	return h, nil
}

func (b *Backend) readRun(handle journal.RunHandle) (*runOwner, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.runs[handle]
	if r == nil {
		return nil, journal.ErrClosed
	}
	return r, nil
}
func (b *Backend) writeRun(handle journal.RunHandle, token journal.OwnerToken) (*runOwner, error) {
	r, err := b.readRun(handle)
	if err != nil {
		return nil, err
	}
	if token == "" || r.token != token {
		return nil, errors.New("journalpg: stale run owner token")
	}
	return r, nil
}
func normalizedOptions(o journal.RunWriterOptions) journal.RunWriterOptions {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Scrubber == nil {
		o.Scrubber = journal.NewPatternScrubber()
	}
	return o
}
func (b *Backend) keepRun(r *runOwner) journal.RunHandle {
	handle := journal.RunHandle(rand.Text())
	b.mu.Lock()
	b.runs[handle] = r
	b.mu.Unlock()
	return handle
}

// withRun locks the narrow counter row before any append/dedup decision. Every
// subsequent write also predicates on the live epoch and this incarnation.
func (b *Backend) withRun(ctx context.Context, r *runOwner, write func(context.Context, pgx.Tx, int64) error) error {
	h, err := b.liveOwner()
	if err != nil {
		return err
	}
	if h.Epoch != r.epoch {
		return lease.ErrStale
	}
	return b.config.Lease.WithFence(ctx, h, func(ctx context.Context, tx pgx.Tx) error {
		var seq int64
		err := tx.QueryRow(ctx, `SELECT last_seq FROM journal.run_seq WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4 FOR UPDATE`, b.config.InstanceID, r.key.RunID, r.token, h.Epoch).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return lease.ErrStale
		}
		if err != nil {
			return err
		}
		return write(ctx, tx, seq)
	})
}
func (b *Backend) changed(r *runOwner, seq uint64) {
	select {
	case b.dirty <- struct{}{}:
	default:
	}
	if r.options.AppendObserver != nil {
		r.options.AppendObserver(r.key.RunID, seq)
	}
	if r.options.ContextObserver != nil {
		ctx := r.options.ObserverContext
		if ctx == nil {
			ctx = context.Background()
		}
		r.options.ContextObserver(ctx, r.key.RunID, seq)
	}
}
func requireWrite(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("journalpg: %w", lease.ErrStale)
	}
	return nil
}
