package readmodelpg

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const changeChannel = "goobers_readmodel_change"

// Notifier fans one dedicated PostgreSQL LISTEN connection out to local feed
// subscribers. Feed polling remains the correctness fallback.
type Notifier struct {
	listener  *pgx.Conn
	publisher *pgx.Conn

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
	closed      bool
	publishMu   sync.Mutex
	closeOnce   sync.Once
}

// NewNotifier opens dedicated non-pooled LISTEN and NOTIFY connections.
func NewNotifier(ctx context.Context, pool *pgxpool.Pool) (*Notifier, error) {
	if pool == nil {
		return nil, errors.New("readmodelpg: notifier pool is required")
	}
	listener, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, fmt.Errorf("readmodelpg: connect notifier listener: %w", err)
	}
	publisher, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		_ = listener.Close(context.Background())
		return nil, fmt.Errorf("readmodelpg: connect notifier publisher: %w", err)
	}
	if _, err := listener.Exec(ctx, `LISTEN `+changeChannel); err != nil {
		_ = publisher.Close(context.Background())
		_ = listener.Close(context.Background())
		return nil, fmt.Errorf("readmodelpg: listen for changes: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	notifier := &Notifier{
		listener: listener, publisher: publisher,
		ctx: runCtx, cancel: cancel, done: make(chan struct{}),
		subscribers: make(map[chan struct{}]struct{}),
	}
	go notifier.listen()
	return notifier, nil
}

// Subscribe registers one feed wait. The returned unregister is idempotent.
func (n *Notifier) Subscribe() (<-chan struct{}, func()) {
	waiter := make(chan struct{})
	n.mu.Lock()
	if n.closed {
		close(waiter)
		n.mu.Unlock()
		return waiter, func() {}
	}
	n.subscribers[waiter] = struct{}{}
	n.mu.Unlock()
	var once sync.Once
	return waiter, func() {
		once.Do(func() {
			n.mu.Lock()
			delete(n.subscribers, waiter)
			n.mu.Unlock()
		})
	}
}

// Notify publishes a coalescible wakeup after a projection commit.
func (n *Notifier) Notify(ctx context.Context) error {
	n.publishMu.Lock()
	defer n.publishMu.Unlock()
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return errors.New("readmodelpg: notifier is closed")
	}
	if _, err := n.publisher.Exec(ctx, `SELECT pg_notify($1, '')`, changeChannel); err != nil {
		return fmt.Errorf("readmodelpg: notify change feed: %w", err)
	}
	return nil
}

func (n *Notifier) listen() {
	defer close(n.done)
	for {
		if _, err := n.listener.WaitForNotification(n.ctx); err != nil {
			n.broadcast()
			return
		}
		n.broadcast()
	}
}

func (n *Notifier) broadcast() {
	n.mu.Lock()
	subscribers := n.subscribers
	n.subscribers = make(map[chan struct{}]struct{})
	n.mu.Unlock()
	for subscriber := range subscribers {
		close(subscriber)
	}
}

// Close stops the listener and releases both dedicated connections.
func (n *Notifier) Close() error {
	var result error
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		n.mu.Unlock()
		n.cancel()
		<-n.done
		n.broadcast()
		n.publishMu.Lock()
		result = errors.Join(
			n.publisher.Close(context.Background()),
			n.listener.Close(context.Background()),
		)
		n.publishMu.Unlock()
	})
	return result
}
