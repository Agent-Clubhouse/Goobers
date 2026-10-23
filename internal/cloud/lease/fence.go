package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WithFence executes one bounded transaction while share-locking the acquired
// lease epoch. A takeover cannot commit until this transaction finishes.
func (m *Manager) WithFence(ctx context.Context, handle *Handle, write func(context.Context, pgx.Tx) error) error {
	if handle == nil || write == nil {
		return errors.New("cloud fenced transaction requires a lease handle and write")
	}
	remaining := m.selfFenceRemaining(handle)
	if remaining <= 0 {
		return ErrSelfFenced
	}
	timeout := min(FencedTransactionTimeout, remaining)
	timeoutCause := ErrFencedTransactionTimeout
	if remaining <= FencedTransactionTimeout {
		timeoutCause = ErrSelfFenced
	}
	txCtx, cancel := context.WithTimeoutCause(ctx, timeout, timeoutCause)
	defer cancel()
	tx, err := m.config.Pool.BeginTx(txCtx, pgx.TxOptions{})
	if err != nil {
		if deadlineErr := fencedDeadlineError(txCtx); deadlineErr != nil {
			return deadlineErr
		}
		return fmt.Errorf("begin fenced transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(txCtx) }()
	timeoutSetting := fmt.Sprintf("%dms", max(int64(1), timeout.Milliseconds()))
	for _, setting := range []string{"statement_timeout", "idle_in_transaction_session_timeout"} {
		if _, err := tx.Exec(txCtx, "SELECT set_config($1, $2, true)", setting, timeoutSetting); err != nil {
			if deadlineErr := fencedDeadlineError(txCtx); deadlineErr != nil {
				return deadlineErr
			}
			return fmt.Errorf("bound fenced transaction: %w", err)
		}
	}
	if err := shareEpoch(txCtx, tx, handle); err != nil {
		if deadlineErr := fencedDeadlineError(txCtx); deadlineErr != nil {
			return deadlineErr
		}
		return err
	}
	if err := write(txCtx, tx); err != nil {
		if deadlineErr := fencedDeadlineError(txCtx); deadlineErr != nil {
			return deadlineErr
		}
		return err
	}
	if deadlineErr := fencedDeadlineError(txCtx); deadlineErr != nil {
		return deadlineErr
	}
	if m.SelfFenced(handle) {
		return ErrSelfFenced
	}
	if err := tx.Commit(txCtx); err != nil {
		if deadlineErr := fencedDeadlineError(txCtx); deadlineErr != nil {
			return deadlineErr
		}
		return fmt.Errorf("commit fenced transaction: %w", err)
	}
	return nil
}

// Confirm share-locks the acquired epoch inside a caller-owned transaction
// that is about to commit. Unlike WithFence it does not bound the
// transaction, so long-running work (such as migration DDL) can run first and
// confirm ownership only at its end: a takeover that already committed makes
// Confirm fail, and one still pending waits for this transaction to finish.
func (m *Manager) Confirm(ctx context.Context, tx pgx.Tx, handle *Handle) error {
	if handle == nil || tx == nil {
		return errors.New("cloud lease confirmation requires a lease handle and transaction")
	}
	if m.SelfFenced(handle) {
		return ErrSelfFenced
	}
	if err := shareEpoch(ctx, tx, handle); err != nil {
		return err
	}
	if m.SelfFenced(handle) {
		return ErrSelfFenced
	}
	return nil
}

func shareEpoch(ctx context.Context, tx pgx.Tx, handle *Handle) error {
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM control.lease
		WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4
		FOR SHARE`,
		handle.InstanceID, handle.Name, handle.Holder, handle.Epoch,
	).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStale
	}
	if err != nil {
		return fmt.Errorf("validate fenced transaction: %w", err)
	}
	return nil
}

func fencedDeadlineError(ctx context.Context) error {
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrSelfFenced) || errors.Is(cause, ErrFencedTransactionTimeout) {
		return cause
	}
	return nil
}

// Run holds and renews the lease for work. Context cancellation triggers a
// voluntary release using a fresh bounded context, including on SIGTERM.
func (m *Manager) Run(ctx context.Context, work func(context.Context, *Handle) error) error {
	if work == nil {
		return errors.New("cloud lease work is required")
	}
	handle, err := m.Acquire(ctx)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	renewalDone := make(chan error, 1)
	go func() {
		renewalDone <- m.maintain(runCtx, cancel, handle)
	}()
	workErr := work(runCtx, handle)
	cancel()
	renewalErr := <-renewalDone
	// A timed-out callback or failed self-fence must not grant an early handoff.
	// Stop renewal and let the database TTL expire instead.
	if errors.Is(workErr, ErrWorkStillRunning) || errors.Is(renewalErr, ErrSelfFenced) {
		return errors.Join(workErr, renewalErr)
	}

	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), FencedTransactionTimeout)
	defer releaseCancel()
	releaseErr := m.Release(releaseCtx, handle)
	switch {
	case renewalErr != nil:
		return renewalErr
	case workErr != nil:
		return workErr
	case releaseErr != nil:
		return releaseErr
	default:
		return nil
	}
}

func (m *Manager) maintain(ctx context.Context, cancel context.CancelFunc, handle *Handle) error {
	return m.maintainRenewals(ctx, cancel, handle, m.Renew)
}

func (m *Manager) maintainRenewals(ctx context.Context, cancel context.CancelFunc, handle *Handle, renew func(context.Context, *Handle) error) error {
	retry := false
	for {
		remaining := m.selfFenceRemaining(handle)
		if remaining <= 0 {
			cancel()
			return fmt.Errorf("%w: last renewal exceeded %s", ErrSelfFenced, m.config.SelfFenceAfter)
		}
		wait := min(m.config.RenewInterval, remaining)
		if retry {
			// Leave time for another attempt without moving the last-success deadline.
			wait = min(max(time.Millisecond, min(m.config.RenewInterval/5, remaining/2)), remaining)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-m.config.After(wait):
		}
		if m.SelfFenced(handle) {
			cancel()
			return fmt.Errorf("%w: last renewal exceeded %s", ErrSelfFenced, m.config.SelfFenceAfter)
		}
		renewCtx, renewCancel := context.WithCancel(ctx)
		renewed := make(chan error, 1)
		go func() {
			renewed <- renew(renewCtx, handle)
		}()
		remaining = m.selfFenceRemaining(handle)
		select {
		case <-ctx.Done():
			renewCancel()
			return nil
		case <-m.config.After(remaining):
			renewCancel()
			cancel()
			return fmt.Errorf("%w: last renewal exceeded %s", ErrSelfFenced, m.config.SelfFenceAfter)
		case err := <-renewed:
			renewCancel()
			if errors.Is(err, ErrStale) {
				cancel()
				return err
			}
			retry = err != nil
			// Transient renew failures retry, but the next loop is capped by
			// the unchanged last-success self-fence deadline.
		}
	}
}
