//go:build cloudintegration

package lease_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// commitKiller terminates a session's backend just as it sends COMMIT, once
// armed, so the client never learns whether the transaction committed.
type commitKiller struct {
	admin *pgxpool.Pool
	armed atomic.Bool
	fired atomic.Bool
}

func (k *commitKiller) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if k.armed.Load() && strings.EqualFold(strings.TrimSpace(data.SQL), "commit") && k.fired.CompareAndSwap(false, true) {
		_, _ = k.admin.Exec(context.WithoutCancel(ctx), "SELECT pg_terminate_backend($1)", conn.PgConn().PID())
	}
	return ctx
}

func (*commitKiller) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestFencedWriteReportsATerminatedCommitAsUnknown: a session-ending server
// error at COMMIT (here the FATAL 57P01 the client receives when its backend
// is terminated as COMMIT is sent) is not proof of a rollback, and must read
// as ErrCommitUnknown rather than a definite failure.
func TestFencedWriteReportsATerminatedCommitAsUnknown(t *testing.T) {
	pool := testPool(t)
	killer := &commitKiller{admin: pool}
	config := pool.Config().Copy()
	config.ConnConfig.Tracer = killer
	traced, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	manager, err := lease.New(lease.Config{Pool: traced, InstanceID: "instance", Name: "control", Holder: "holder-a"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	killer.armed.Store(true)
	err = manager.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT 1")
		return err
	})
	if !killer.fired.Load() {
		t.Fatal("precondition: the fenced write's COMMIT was never sent")
	}
	if !errors.Is(err, lease.ErrCommitUnknown) || errors.Is(err, lease.ErrSelfFenced) {
		t.Fatalf("fenced write with a terminated COMMIT = %v, want ErrCommitUnknown", err)
	}
}
