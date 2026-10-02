package journal

import (
	"context"
	"sync"
	"time"
)

// WithAsyncAppendObserver maintains one coalesced derived-state watermark per
// open run. The callback never runs under the journal mutex. Its context is
// canceled with the owning runner and each observation receives a five-second
// deadline. The callback must honor that context, including during shutdown.
// The callback owns failure reporting; the durable journal remains repairable.
func WithAsyncAppendObserver(ctx context.Context, observe func(context.Context, string, uint64)) Option {
	return func(c *config) {
		c.observerContext, c.asyncObserver = ctx, observe
	}
}

type appendObserver struct {
	mu      sync.Mutex
	latest  uint64
	closed  bool
	wake    chan struct{}
	done    chan struct{}
	ctx     context.Context
	runID   string
	observe func(context.Context, string, uint64)
}

func (r *Run) configureObserver(c config) {
	if c.asyncObserver == nil {
		return
	}
	ctx := c.observerContext
	if ctx == nil {
		ctx = context.Background()
	}
	o := &appendObserver{ctx: ctx, runID: r.id.RunID, observe: c.asyncObserver, wake: make(chan struct{}, 1), done: make(chan struct{})}
	r.pendingObserver = o
	r.observerStartSeq = r.seq
	r.observer = o.enqueue
	go o.run()
}

func (o *appendObserver) enqueue(_ string, seq uint64) {
	o.mu.Lock()
	if !o.closed && seq > o.latest {
		o.latest = seq
	}
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *appendObserver) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
	<-o.done
}

func (o *appendObserver) run() {
	defer close(o.done)
	var observed uint64
	for {
		<-o.wake
		o.mu.Lock()
		seq, closed := o.latest, o.closed
		o.mu.Unlock()
		if seq > observed {
			ctx, cancel := context.WithTimeout(o.ctx, 5*time.Second)
			o.observe(ctx, o.runID, seq)
			cancel()
			observed = seq
		}
		if closed {
			return
		}
	}
}
