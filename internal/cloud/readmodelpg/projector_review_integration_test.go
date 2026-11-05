//go:build cloudintegration

package readmodelpg_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestFeedAfterFlipReportsEpochChanged(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	fence := testFence{pool: pool, epoch: 7}
	before, err := readmodelpg.New(t.Context(), pool, fence)
	if err != nil {
		t.Fatal(err)
	}
	feed := readmodel.NewFeedWithWakeup(
		readmodelpg.NewActiveFeedStore(pool), nil)
	if err := before.UpsertRun(t.Context(), testProjection("v1", 1)); err != nil {
		t.Fatal(err)
	}
	head, err := feed.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	builder, _ := readmodelpg.NewBuilder(pool, fence)
	if err := builder.Build(t.Context(), "readmodel_v2"); err != nil {
		t.Fatal(err)
	}
	if err := builder.Flip(t.Context(), "readmodel", 2); err != nil {
		t.Fatal(err)
	}
	_, err = feed.Since(t.Context(), head, 10)
	var cursorErr *readmodel.CursorError
	if !errors.As(err, &cursorErr) ||
		cursorErr.Condition != readmodel.CursorEpochChanged {
		t.Fatalf("Since old epoch after flip = %v, want epoch_changed", err)
	}
}

type countingFeedStore struct {
	*readmodelpg.Store
	reads atomic.Int32
}

func (s *countingFeedStore) Changes(
	ctx context.Context,
	after uint64,
	limit int,
) ([]readmodel.Change, error) {
	s.reads.Add(1)
	return s.Store.Changes(ctx, after, limit)
}

func TestClosedNotifierFallsBackToPolling(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	store, err := readmodelpg.New(t.Context(), pool, testFence{pool: pool, epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	notifier, err := readmodelpg.NewNotifier(t.Context(), notifierConfig(pool))
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingFeedStore{Store: store}
	feed := readmodel.NewFeedWithWakeup(counting, notifier)
	head, err := feed.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	_, _ = feed.Since(ctx, head, 10)
	if reads := counting.reads.Load(); reads > 3 {
		t.Fatalf("closed notifier caused %d reads, want <=3", reads)
	}
}

func TestNotifierReconnectsPublisherAndListener(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	config := notifierConfig(pool)
	config.ReconnectMin = 20 * time.Millisecond
	config.ReconnectMax = 100 * time.Millisecond
	notifier, err := readmodelpg.NewNotifier(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = notifier.Close() }()
	projector, _, _ := testProjector(
		t, pool, "notifier-reconnect-"+t.Name(), "holder", notifier)
	if err := projector.Commit(t.Context(), testProjection("first", 1)); err != nil {
		t.Fatal(err)
	}
	publisherPID := notifierBackendPID(t, pool, "%pg_notify%")
	listenerPID := notifierBackendPID(t, pool, "LISTEN %")
	firstWake, unregister := notifier.Subscribe()
	defer unregister()
	if _, err := pool.Exec(t.Context(),
		`SELECT pg_terminate_backend($1)`, listenerPID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstWake:
	case <-time.After(2 * time.Second):
		t.Fatal("listener loss did not wake subscribers")
	}
	if _, err := pool.Exec(t.Context(),
		`SELECT pg_terminate_backend($1)`, publisherPID); err != nil {
		t.Fatal(err)
	}
	if err := projector.Commit(t.Context(), testProjection("second", 1)); err != nil {
		t.Fatalf("committed projection surfaced NOTIFY failure: %v", err)
	}
	waitForNotifierHealth(t, notifier)
	waiter, unregister2 := notifier.Subscribe()
	defer unregister2()
	if err := notifier.Notify(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiter:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnected notifier did not wake subscriber")
	}
}

func notifierBackendPID(
	t *testing.T,
	pool *pgxpool.Pool,
	query string,
) int {
	t.Helper()
	var pid int
	if err := pool.QueryRow(t.Context(), `
		SELECT pid FROM pg_stat_activity
		WHERE query LIKE $1 AND pid<>pg_backend_pid()
		ORDER BY backend_start DESC LIMIT 1`, query,
	).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitForNotifierHealth(t *testing.T, notifier *readmodelpg.Notifier) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !notifier.Health().Degraded() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("notifier did not recover: %+v", notifier.Health())
}

type freezeNotifierProxy struct {
	listener net.Listener
	frozen   atomic.Bool
}

func newFreezeNotifierProxy(t *testing.T, target string) *freezeNotifierProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &freezeNotifierProxy{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			pipe := func(destination net.Conn, source net.Conn) {
				buffer := make([]byte, 32*1024)
				for {
					count, err := source.Read(buffer)
					if err != nil {
						return
					}
					if proxy.frozen.Load() {
						continue
					}
					if _, err := destination.Write(buffer[:count]); err != nil {
						return
					}
				}
			}
			go pipe(server, client)
			go pipe(client, server)
		}
	}()
	return proxy
}

func TestCommitDoesNotBlockOnBlackholedNotifier(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	direct := pool.Config().ConnConfig.Copy()
	proxy := newFreezeNotifierProxy(
		t, fmt.Sprintf("%s:%d", direct.Host, direct.Port))
	address := proxy.listener.Addr().(*net.TCPAddr)
	direct.Host, direct.Port = "127.0.0.1", uint16(address.Port)
	notifier, err := readmodelpg.NewNotifier(
		t.Context(), readmodelpg.NotifierConfig{
			Direct: direct, NotifyTimeout: 100 * time.Millisecond,
		})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { go func() { _ = notifier.Close() }() }()
	projector, _, _ := testProjector(
		t, pool, "notifier-blackhole-"+t.Name(), "holder", notifier)
	proxy.frozen.Store(true)
	done := make(chan error, 1)
	go func() {
		done <- projector.Commit(
			context.Background(), testProjection("blackholed", 1))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_, committed, _ := projector.Store().GetRun(
			t.Context(), "blackholed")
		t.Fatalf("Commit blocked on black-holed NOTIFY (committed=%v)", committed)
	}
}
