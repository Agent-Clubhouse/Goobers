//go:build cloudintegration

package journal_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/testdep"
)

// mintFault reproduces the sync-rep COMMIT uncertainty from review-8160.
// Only the faulted transaction enables synchronous_commit; admin sessions
// remain asynchronous and the fixture does not run a background renewal. The kill waits for PG's SyncRep state, not a clock guess.
type mintFault struct {
	admin        *pgxpool.Pool
	armed        atomic.Bool
	fired        atomic.Bool
	failRead     atomic.Bool
	beforeCommit bool
	priorStandby string
	done         chan error
}

func (f *mintFault) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT owner_token,owner_epoch FROM journal.run_seq") && f.failRead.CompareAndSwap(true, false) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return cancelled
	}
	if !strings.EqualFold(strings.TrimSpace(data.SQL), "commit") || !f.armed.Load() || !f.fired.CompareAndSwap(false, true) {
		return ctx
	}
	if f.beforeCommit {
		_, err := f.admin.Exec(ctx, "SELECT pg_terminate_backend($1)", conn.PgConn().PID())
		// Force observation before the driver sends COMMIT.
		_ = conn.Close(ctx)
		f.done <- err
		return ctx
	}
	if _, err := conn.Exec(ctx, "SET LOCAL synchronous_commit=on"); err != nil {
		f.done <- err
		return ctx
	}
	go f.killAtSyncRep(conn.PgConn().PID())
	return ctx
}

func (*mintFault) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (f *mintFault) killAtSyncRep(pid uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var waiting bool
		err := f.admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event='SyncRep')", pid).Scan(&waiting)
		if err != nil {
			f.done <- err
			return
		}
		if waiting {
			_, err = f.admin.Exec(ctx, "SELECT pg_terminate_backend($1)", pid)
			f.done <- err
			return
		}
		select {
		case <-ctx.Done():
			f.done <- ctx.Err()
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func mintFaultBackend(t *testing.T, beforeCommit bool) (*journalpg.Backend, journalpg.Config, *mintFault) {
	t.Helper()
	testdep.RequireEnv(t, "GOOBERS_TEST_POSTGRES_URL")
	base := postgresJournalConfigRenew(t, false)
	adminConfig := base.Pool.Config()
	adminConfig.AfterConnect = nil
	adminConfig.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	admin, err := pgxpool.NewWithConfig(t.Context(), adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	f := &mintFault{admin: admin, beforeCommit: beforeCommit, done: make(chan error, 1)}
	// Install the stand-by wait only after migrations and lease acquisition.
	// Other sessions use the server's normal behavior; reset is registered before
	// any fallible test operation, and the suite is deliberately not parallel.
	var prior string
	if err := admin.QueryRow(t.Context(), "SHOW synchronous_standby_names").Scan(&prior); err != nil {
		t.Fatal(err)
	}
	f.priorStandby = prior
	reset := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "ALTER SYSTEM SET synchronous_standby_names = "+quotePGSetting(prior)); err != nil {
			t.Error(err)
		}
		if _, err := admin.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(reset)
	if !beforeCommit {
		if _, err := admin.Exec(t.Context(), "ALTER SYSTEM SET synchronous_standby_names='journal_mint_absent_standby'"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(t.Context(), "SELECT pg_reload_conf()"); err != nil {
			t.Fatal(err)
		}
		waitMintStandby(t, admin)
	}
	cfg := base.Pool.Config()
	cfg.ConnConfig.Tracer = f
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var role string
	if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("runtime role=%q: %v", role, err)
	}
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: base.InstanceID, Name: base.LiveOwner().Name, Holder: base.LiveOwner().Holder})
	if err != nil {
		t.Fatal(err)
	}
	base.Pool, base.Lease = pool, manager
	return postgresBackend(t, base), base, f
}

func quotePGSetting(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func waitMintStandby(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var setting string
		if err := pool.QueryRow(ctx, "SHOW synchronous_standby_names").Scan(&setting); err != nil {
			t.Fatal(err)
		}
		if setting == "journal_mint_absent_standby" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (f *mintFault) finish(t *testing.T) {
	t.Helper()
	select {
	case err := <-f.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("COMMIT fault never fired")
	}
	if !f.fired.Load() {
		t.Fatal("COMMIT fault not exercised")
	}
	if _, err := f.admin.Exec(t.Context(), "ALTER SYSTEM SET synchronous_standby_names = "+quotePGSetting(f.priorStandby)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(t.Context(), "SELECT pg_reload_conf()"); err != nil {
		t.Fatal(err)
	}
}

var _ pgx.QueryTracer = (*mintFault)(nil)
