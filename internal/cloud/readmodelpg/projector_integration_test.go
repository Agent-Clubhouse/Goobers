//go:build cloudintegration

package readmodelpg_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestProjectorFencedCommit(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	instanceID := "projector-fence-" + t.Name()
	old, _, oldHandle := testProjector(t, pool, instanceID, "old", nil)
	if err := old.Commit(t.Context(), testProjection("old-commit", 1)); err != nil {
		t.Fatal(err)
	}
	expireProjectorLease(t, pool, oldHandle)
	current, _, _ := testProjector(t, pool, instanceID, "current", nil)
	if err := old.Commit(t.Context(), testProjection("stale-commit", 1)); !errors.Is(err, cloudlease.ErrStale) {
		t.Fatalf("stale projector commit = %v, want ErrStale", err)
	}
	if err := current.Commit(t.Context(), testProjection("current-commit", 1)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := current.Store().GetRun(t.Context(), "stale-commit"); err != nil || ok {
		t.Fatalf("stale projection committed: ok=%v err=%v", ok, err)
	}
	changes, err := current.Store().Changes(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Seq+1 != changes[1].Seq {
		t.Fatalf("changes = %+v, want two dense committed changes", changes)
	}
}

func TestFeedWakesWithoutNotify(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	store, err := readmodelpg.New(t.Context(), pool, testFence{pool: pool, epoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	feed := readmodel.NewFeedWithWakeup(store, silentWakeup{})
	head, err := feed.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		page, err := feed.Since(ctx, head, 10)
		if err == nil && (len(page.Changes) != 1 || page.Changes[0].RunID != "poll-fallback") {
			err = fmt.Errorf("unexpected feed page: %+v", page)
		}
		result <- err
	}()
	if err := store.UpsertRun(t.Context(), testProjection("poll-fallback", 1)); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("feed did not wake through poll fallback: %v", err)
	}
}

func TestNotifierWakesSubscribers(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	notifier, err := readmodelpg.NewNotifier(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = notifier.Close() }()
	wakeup, unregister := notifier.Subscribe()
	defer unregister()
	if err := notifier.Notify(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wakeup:
	case <-time.After(2 * time.Second):
		t.Fatal("PostgreSQL notification did not wake subscriber")
	}
}

func TestSSEResumesAcrossRestart(t *testing.T) {
	pool := testPool(t)
	applyMigrations(t, pool)
	resetReadmodel(t, pool)
	instanceID := "projector-restart-" + t.Name()
	serviceCtx, stopService := context.WithCancel(t.Context())
	notifier, err := readmodelpg.NewNotifier(serviceCtx, pool)
	if err != nil {
		t.Fatal(err)
	}
	first, _, firstHandle := testProjector(t, pool, instanceID, "first", notifier)
	feed := first.Feed()
	head, err := feed.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(t.Context(), testProjection("before-restart", 1)); err != nil {
		t.Fatal(err)
	}
	page, err := feed.Since(t.Context(), head, 10)
	if err != nil {
		t.Fatal(err)
	}
	resume := page.Cursor
	stopService()
	if err := notifier.Close(); err != nil {
		t.Fatal(err)
	}
	expireProjectorLease(t, pool, firstHandle)

	restartedCtx, stopRestarted := context.WithCancel(t.Context())
	defer stopRestarted()
	restartedNotifier, err := readmodelpg.NewNotifier(restartedCtx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restartedNotifier.Close() }()
	restarted, _, _ := testProjector(t, pool, instanceID, "restarted", restartedNotifier)
	if err := restarted.Commit(t.Context(), testProjection("after-restart", 1)); err != nil {
		t.Fatal(err)
	}
	resumed, err := restarted.Feed().Since(t.Context(), resume, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Changes) != 1 || resumed.Changes[0].RunID != "after-restart" {
		t.Fatalf("resumed changes = %+v", resumed.Changes)
	}
	if resumed.Cursor.Epoch != resume.Epoch || resumed.Cursor.Seq <= resume.Seq {
		t.Fatalf("resume cursor = %v, prior = %v", resumed.Cursor, resume)
	}
}

type silentWakeup struct{}

func (silentWakeup) Subscribe() (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

func testProjector(
	t *testing.T,
	pool *pgxpool.Pool,
	instanceID string,
	holder string,
	notifier *readmodelpg.Notifier,
) (*readmodelpg.Projector, *cloudlease.Manager, *cloudlease.Handle) {
	t.Helper()
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: instanceID, Name: "projector", Holder: holder,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	projector, err := readmodelpg.NewProjector(t.Context(), pool, manager, handle, notifier)
	if err != nil {
		t.Fatal(err)
	}
	return projector, manager, handle
}

func expireProjectorLease(
	t *testing.T,
	pool *pgxpool.Pool,
	handle *cloudlease.Handle,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.lease_heartbeat
		SET expires_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4`,
		handle.InstanceID, handle.Name, handle.Holder, handle.Epoch,
	); err != nil {
		t.Fatal(err)
	}
}
