package readmodelpg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	changeChannel         = "goobers_readmodel_change"
	defaultNotifyCoalesce = 250 * time.Millisecond
	defaultReconnectMin   = 100 * time.Millisecond
	defaultReconnectMax   = 5 * time.Second
)

// NotifierConfig requires a direct PostgreSQL connection. LISTEN is not valid
// through a transaction-pooled PgBouncer endpoint.
type NotifierConfig struct {
	Direct       *pgx.ConnConfig
	Logger       *slog.Logger
	Coalesce     time.Duration
	ReconnectMin time.Duration
	ReconnectMax time.Duration
}

// NotifierHealth exposes notification degradation without making wakeups part
// of projection correctness.
type NotifierHealth struct {
	ListenerConnected  bool
	PublisherConnected bool
	LastError          string
}

// Degraded reports whether either dedicated connection is unavailable.
func (h NotifierHealth) Degraded() bool {
	return !h.ListenerConnected || !h.PublisherConnected
}

// Notifier fans one dedicated PostgreSQL LISTEN connection out to local feed
// subscribers and coalesces publishing to at most one NOTIFY per window.
type Notifier struct {
	config NotifierConfig
	ctx    context.Context
	cancel context.CancelFunc
	done   sync.WaitGroup

	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
	closed      bool
	health      NotifierHealth

	listenerMu sync.Mutex
	listener   *pgx.Conn

	publisherMu sync.Mutex
	publisher   *pgx.Conn
	lastPublish time.Time
	pending     chan struct{}

	closeOnce sync.Once
}

// NewNotifier opens initial dedicated direct connections. The caller context
// bounds construction only; background reconnects use the notifier lifecycle.
func NewNotifier(ctx context.Context, config NotifierConfig) (*Notifier, error) {
	if config.Direct == nil {
		return nil, errors.New("readmodelpg: direct notifier connection is required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Coalesce <= 0 {
		config.Coalesce = defaultNotifyCoalesce
	}
	if config.ReconnectMin <= 0 {
		config.ReconnectMin = defaultReconnectMin
	}
	if config.ReconnectMax <= 0 {
		config.ReconnectMax = defaultReconnectMax
	}
	if config.ReconnectMax < config.ReconnectMin {
		return nil, errors.New("readmodelpg: invalid notifier reconnect bounds")
	}
	listener, err := connectListener(ctx, config.Direct)
	if err != nil {
		return nil, err
	}
	publisher, err := pgx.ConnectConfig(ctx, config.Direct.Copy())
	if err != nil {
		_ = listener.Close(context.Background())
		return nil, fmt.Errorf("readmodelpg: connect notifier publisher: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	notifier := &Notifier{
		config: config, ctx: runCtx, cancel: cancel,
		subscribers: make(map[chan struct{}]struct{}),
		listener:    listener, publisher: publisher,
		health: NotifierHealth{
			ListenerConnected: true, PublisherConnected: true,
		},
		pending: make(chan struct{}, 1),
	}
	notifier.done.Add(2)
	go notifier.listenLoop()
	go notifier.publishLoop()
	return notifier, nil
}

func connectListener(ctx context.Context, config *pgx.ConnConfig) (*pgx.Conn, error) {
	connection, err := pgx.ConnectConfig(ctx, config.Copy())
	if err != nil {
		return nil, fmt.Errorf("readmodelpg: connect notifier listener: %w", err)
	}
	if _, err := connection.Exec(ctx, `LISTEN `+changeChannel); err != nil {
		_ = connection.Close(context.Background())
		return nil, fmt.Errorf("readmodelpg: listen for changes: %w", err)
	}
	return connection, nil
}

// Subscribe registers one feed wait. After Close it returns a channel that
// never fires, leaving the one-second poll as the fallback rather than spinning.
func (n *Notifier) Subscribe() (<-chan struct{}, func()) {
	waiter := make(chan struct{})
	n.mu.Lock()
	if n.closed {
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

// Notify issues a best-effort, coalesced latency hint. Projection commits never
// fail because NOTIFY is unavailable; the feed poll is the correctness path.
func (n *Notifier) Notify(ctx context.Context) error {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return nil
	}
	n.publisherMu.Lock()
	var failed *pgx.Conn
	var publishErr error
	if n.publisher != nil && time.Since(n.lastPublish) >= n.config.Coalesce {
		_, publishErr = n.publisher.Exec(ctx, `SELECT pg_notify($1, '')`, changeChannel)
		n.lastPublish = time.Now()
		if publishErr == nil {
			n.publisherMu.Unlock()
			return nil
		}
		failed = n.publisher
		n.publisher = nil
	}
	n.publisherMu.Unlock()
	if failed != nil {
		_ = failed.Close(context.Background())
		n.mu.Lock()
		n.health.PublisherConnected = false
		n.recordErrorLocked(publishErr)
		n.mu.Unlock()
	}
	n.requestPublish()
	return nil
}

func (n *Notifier) requestPublish() {
	select {
	case n.pending <- struct{}{}:
	default:
	}
}

func (n *Notifier) listenLoop() {
	defer n.done.Done()
	backoff := n.config.ReconnectMin
	for {
		n.listenerMu.Lock()
		connection := n.listener
		n.listenerMu.Unlock()
		if connection != nil {
			if _, err := connection.WaitForNotification(n.ctx); err == nil {
				n.broadcast()
				continue
			} else if n.ctx.Err() == nil {
				n.setListener(nil, err)
				n.broadcast()
			}
		}
		if n.ctx.Err() != nil {
			return
		}
		if !waitBackoff(n.ctx, backoff) {
			return
		}
		connection, err := connectListener(n.ctx, n.config.Direct)
		if err != nil {
			n.setListener(nil, err)
			backoff = min(backoff*2, n.config.ReconnectMax)
			continue
		}
		n.setListener(connection, nil)
		backoff = n.config.ReconnectMin
		n.broadcast()
	}
}

func (n *Notifier) publishLoop() {
	defer n.done.Done()
	backoff := n.config.ReconnectMin
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-n.pending:
		}
		n.publisherMu.Lock()
		wait := time.Until(n.lastPublish.Add(n.config.Coalesce))
		n.publisherMu.Unlock()
		if wait > 0 && !waitBackoff(n.ctx, wait) {
			return
		}
		for {
			n.publisherMu.Lock()
			connection := n.publisher
			n.publisherMu.Unlock()
			if connection == nil {
				if !waitBackoff(n.ctx, backoff) {
					return
				}
				reconnected, err := pgx.ConnectConfig(n.ctx, n.config.Direct.Copy())
				if err != nil {
					n.setPublisher(nil, err)
					backoff = min(backoff*2, n.config.ReconnectMax)
					continue
				}
				n.setPublisher(reconnected, nil)
				connection = reconnected
				backoff = n.config.ReconnectMin
			}
			n.publisherMu.Lock()
			_, err := connection.Exec(n.ctx, `SELECT pg_notify($1, '')`, changeChannel)
			n.lastPublish = time.Now()
			n.publisherMu.Unlock()
			if err != nil {
				n.setPublisher(nil, err)
				continue
			}
			break
		}
	}
}

func waitBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (n *Notifier) setListener(connection *pgx.Conn, err error) {
	n.listenerMu.Lock()
	previous := n.listener
	n.listener = connection
	n.listenerMu.Unlock()
	if previous != nil && previous != connection {
		_ = previous.Close(context.Background())
	}
	n.mu.Lock()
	n.health.ListenerConnected = connection != nil
	n.recordErrorLocked(err)
	n.mu.Unlock()
}

func (n *Notifier) setPublisher(connection *pgx.Conn, err error) {
	n.publisherMu.Lock()
	previous := n.publisher
	n.publisher = connection
	n.publisherMu.Unlock()
	if previous != nil && previous != connection {
		_ = previous.Close(context.Background())
	}
	n.mu.Lock()
	n.health.PublisherConnected = connection != nil
	n.recordErrorLocked(err)
	n.mu.Unlock()
}

func (n *Notifier) recordErrorLocked(err error) {
	if err == nil {
		if !n.health.Degraded() {
			n.health.LastError = ""
		}
		return
	}
	n.health.LastError = err.Error()
	n.config.Logger.Warn("read-model notifier degraded", "error", err)
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

// Health reports dedicated connection state.
func (n *Notifier) Health() NotifierHealth {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.health
}

// Close stops reconnect loops and releases dedicated connections.
func (n *Notifier) Close() error {
	var result error
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		n.mu.Unlock()
		n.cancel()
		n.done.Wait()
		n.broadcast()
		n.listenerMu.Lock()
		listener := n.listener
		n.listener = nil
		n.listenerMu.Unlock()
		n.publisherMu.Lock()
		publisher := n.publisher
		n.publisher = nil
		n.publisherMu.Unlock()
		if listener != nil {
			result = errors.Join(result, listener.Close(context.Background()))
		}
		if publisher != nil {
			result = errors.Join(result, publisher.Close(context.Background()))
		}
	})
	return result
}
