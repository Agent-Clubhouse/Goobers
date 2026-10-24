// Package lease implements PostgreSQL-backed leadership leases and write fences.
package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// ControllerName is the lease name of the single controller role. The
	// controller, the journal epoch stamp and the HA invariant checker share it.
	ControllerName = "controller"
	// DefaultTTL is the database-clock lease lifetime.
	DefaultTTL = 15 * time.Second
	// DefaultRenewInterval is the normal heartbeat cadence.
	DefaultRenewInterval = 5 * time.Second
	// DefaultSelfFenceAfter bounds operation without a successful renew.
	DefaultSelfFenceAfter = 10 * time.Second
	// FencedTransactionTimeout bounds the whole transaction, statements, and idle time.
	FencedTransactionTimeout = 2 * time.Second
	// MaxFencedConnections bounds concurrent share locks on a lease row.
	MaxFencedConnections int32 = 4
)

var (
	// ErrWorkStillRunning prevents voluntary handoff when work has not stopped.
	// Callers must exit promptly; authority expires without further renewal.
	ErrWorkStillRunning = errors.New("cloud lease work is still running")
	// ErrHeld means another live holder owns the lease.
	ErrHeld = errors.New("cloud lease is held")
	// ErrStale means the holder or epoch no longer owns the lease.
	ErrStale = errors.New("cloud lease epoch is stale")
	// ErrSelfFenced means renew has not succeeded inside the monotonic safety
	// window. From WithFence it means the transaction did not commit.
	ErrSelfFenced = errors.New("cloud lease self-fenced")
	// ErrFencedTransactionTimeout means a fenced transaction exceeded its
	// bound. From WithFence it means the transaction did not commit.
	ErrFencedTransactionTimeout = errors.New("cloud fenced transaction timed out")
	// ErrCommitUnknown means WithFence sent COMMIT but lost its outcome (the
	// connection failed or the deadline passed while committing): the write
	// may have committed. It deliberately wraps neither ErrSelfFenced nor
	// ErrFencedTransactionTimeout, which promise that nothing committed, so a
	// caller treating those as safe to replay stays correct.
	ErrCommitUnknown = errors.New("cloud fenced transaction commit outcome is unknown")
)

// Config identifies a single named lease.
type Config struct {
	Pool           *pgxpool.Pool
	InstanceID     string
	Name           string
	Holder         string
	TTL            time.Duration
	RenewInterval  time.Duration
	SelfFenceAfter time.Duration
	Now            func() time.Time
	After          func(time.Duration) <-chan time.Time
}

// Manager acquires, renews, releases, and fences one named lease.
type Manager struct {
	config Config
}

// Handle is an acquired epoch and its monotonic renewal state.
type Handle struct {
	InstanceID string
	Name       string
	Holder     string
	Epoch      int64

	mu        sync.RWMutex
	lastRenew time.Time
}

// New validates and constructs a lease manager.
func New(config Config) (*Manager, error) {
	if config.Pool == nil {
		return nil, errors.New("cloud lease pool is required")
	}
	if config.Pool.Config().MaxConns > MaxFencedConnections {
		return nil, fmt.Errorf("cloud lease pool has %d connections, maximum is %d", config.Pool.Config().MaxConns, MaxFencedConnections)
	}
	if config.InstanceID == "" || config.Name == "" || config.Holder == "" {
		return nil, errors.New("cloud lease instance, name, and holder are required")
	}
	if config.TTL == 0 {
		config.TTL = DefaultTTL
	}
	if config.RenewInterval == 0 {
		config.RenewInterval = DefaultRenewInterval
	}
	if config.SelfFenceAfter == 0 {
		config.SelfFenceAfter = DefaultSelfFenceAfter
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.After == nil {
		config.After = time.After
	}
	if config.TTL <= 0 || config.RenewInterval <= 0 || config.SelfFenceAfter <= 0 {
		return nil, errors.New("cloud lease durations must be positive")
	}
	if config.SelfFenceAfter >= config.TTL {
		return nil, errors.New("cloud lease self-fence window must be shorter than the TTL")
	}
	return &Manager{config: config}, nil
}

// Acquire takes over an expired lease using the database clock. The history
// is stamped with clock_timestamp(), the statement instant after the
// heartbeat row lock and the expiry decision, never now(): a takeover
// transaction can begin before the holder expires or releases and decide
// after, and history stamped at transaction start would record an acquisition
// before the predecessor's expiry or release that never happened.
func (m *Manager) Acquire(ctx context.Context) (*Handle, error) {
	tx, err := m.config.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin lease acquisition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := m.ensureRows(ctx, tx); err != nil {
		return nil, err
	}
	var holder string
	var epoch int64
	var expired bool
	err = tx.QueryRow(ctx, `
		SELECT holder, epoch, expires_at < clock_timestamp()
		FROM control.lease_heartbeat
		WHERE instance_id=$1 AND name=$2
		FOR UPDATE`,
		m.config.InstanceID, m.config.Name,
	).Scan(&holder, &epoch, &expired)
	if err != nil {
		return nil, fmt.Errorf("lock lease heartbeat: %w", err)
	}
	if holder != "" && !expired {
		return nil, ErrHeld
	}
	if epoch > 0 && (holder != m.config.Holder || expired) {
		if _, err := tx.Exec(ctx, `
			UPDATE control.lease_history
			SET released_at=clock_timestamp(), release_kind='expired'
			WHERE instance_id=$1 AND name=$2 AND epoch=$3 AND released_at IS NULL`,
			m.config.InstanceID, m.config.Name, epoch,
		); err != nil {
			return nil, fmt.Errorf("close expired lease history: %w", err)
		}
	}
	var newEpoch int64
	err = tx.QueryRow(ctx, `
		UPDATE control.lease
		SET holder=$3, epoch=epoch+1
		WHERE instance_id=$1 AND name=$2
		RETURNING epoch`,
		m.config.InstanceID, m.config.Name, m.config.Holder,
	).Scan(&newEpoch)
	if err != nil {
		return nil, fmt.Errorf("take over lease: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.lease_history
			(instance_id, name, epoch, holder, acquired_at)
		VALUES ($1, $2, $3, $4, clock_timestamp())`,
		m.config.InstanceID, m.config.Name, newEpoch, m.config.Holder,
	); err != nil {
		return nil, fmt.Errorf("record lease acquisition: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE control.lease_heartbeat
		SET holder=$3, epoch=$4, expires_at=clock_timestamp()+$5
		WHERE instance_id=$1 AND name=$2`,
		m.config.InstanceID, m.config.Name, m.config.Holder, newEpoch, m.config.TTL,
	); err != nil {
		return nil, fmt.Errorf("initialize lease heartbeat: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit lease acquisition: %w", err)
	}
	var valid bool
	if err := m.config.Pool.QueryRow(ctx, `
		SELECT holder=$3 AND epoch=$4 AND expires_at > clock_timestamp()
		FROM control.lease_heartbeat
		WHERE instance_id=$1 AND name=$2`,
		m.config.InstanceID, m.config.Name, m.config.Holder, newEpoch,
	).Scan(&valid); err != nil {
		return nil, fmt.Errorf("verify acquired lease expiry: %w", err)
	}
	if !valid {
		return nil, ErrStale
	}
	return &Handle{
		InstanceID: m.config.InstanceID, Name: m.config.Name,
		Holder: m.config.Holder, Epoch: newEpoch, lastRenew: m.config.Now(),
	}, nil
}

func (m *Manager) ensureRows(ctx context.Context, tx pgx.Tx) error {
	return EnsureRows(ctx, tx, m.config.InstanceID, m.config.Name)
}

// EnsureRows creates the unheld lease rows for one named lease. It is
// idempotent and is the only place the first-row shape is defined, so cloud
// bootstrap and Acquire cannot diverge.
func EnsureRows(ctx context.Context, tx pgx.Tx, instanceID, name string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.lease_heartbeat (instance_id, name, holder, epoch, expires_at)
		VALUES ($1, $2, '', 0, '-infinity') ON CONFLICT DO NOTHING`,
		instanceID, name,
	); err != nil {
		return fmt.Errorf("initialize lease heartbeat: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.lease (instance_id, name, holder, epoch)
		VALUES ($1, $2, '', 0) ON CONFLICT DO NOTHING`,
		instanceID, name,
	); err != nil {
		return fmt.Errorf("initialize lease authority: %w", err)
	}
	return nil
}

// Renew extends only the heartbeat row and records a monotonic success time.
func (m *Manager) Renew(ctx context.Context, handle *Handle) error {
	tag, err := m.config.Pool.Exec(ctx, `
		UPDATE control.lease_heartbeat
		SET expires_at=clock_timestamp()+$5
		WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4`,
		handle.InstanceID, handle.Name, handle.Holder, handle.Epoch, m.config.TTL,
	)
	if err != nil {
		return fmt.Errorf("renew cloud lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	handle.mu.Lock()
	handle.lastRenew = m.config.Now()
	handle.mu.Unlock()
	return nil
}

// SelfFenced reports whether the monotonic renew safety window elapsed.
func (m *Manager) SelfFenced(handle *Handle) bool {
	return m.selfFenceRemaining(handle) <= 0
}

func (m *Manager) selfFenceRemaining(handle *Handle) time.Duration {
	handle.mu.RLock()
	lastRenew := handle.lastRenew
	handle.mu.RUnlock()
	return m.config.SelfFenceAfter - m.config.Now().Sub(lastRenew)
}

// Release voluntarily gives up authority and bumps the epoch.
func (m *Manager) Release(ctx context.Context, handle *Handle) error {
	tx, err := m.config.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin lease release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var heartbeatHolder string
	var heartbeatEpoch int64
	err = tx.QueryRow(ctx, `
		SELECT holder, epoch
		FROM control.lease_heartbeat
		WHERE instance_id=$1 AND name=$2
		FOR UPDATE`,
		handle.InstanceID, handle.Name,
	).Scan(&heartbeatHolder, &heartbeatEpoch)
	if err != nil {
		return fmt.Errorf("lock lease heartbeat for release: %w", err)
	}
	if heartbeatHolder != handle.Holder || heartbeatEpoch != handle.Epoch {
		return ErrStale
	}
	tag, err := tx.Exec(ctx, `
		UPDATE control.lease
		SET holder='', epoch=epoch+1
		WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4`,
		handle.InstanceID, handle.Name, handle.Holder, handle.Epoch,
	)
	if err != nil {
		return fmt.Errorf("release lease authority: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	if _, err := tx.Exec(ctx, `
		UPDATE control.lease_heartbeat
		SET holder='', epoch=$5, expires_at='-infinity'
		WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4`,
		handle.InstanceID, handle.Name, handle.Holder, handle.Epoch, handle.Epoch+1,
	); err != nil {
		return fmt.Errorf("release lease heartbeat: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE control.lease_history
		SET released_at=clock_timestamp(), release_kind='voluntary'
		WHERE instance_id=$1 AND name=$2 AND epoch=$3 AND released_at IS NULL`,
		handle.InstanceID, handle.Name, handle.Epoch,
	); err != nil {
		return fmt.Errorf("record voluntary lease release: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit lease release: %w", err)
	}
	return nil
}
