//go:build cloudintegration

package lease_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
)

// TestReleaseReportsALostCommitAsUnknown: a release whose COMMIT reply is lost
// may or may not have handed the lease off. It must read as
// ErrCommitUnknown, as WithFence's does, and never as ErrSelfFenced, which
// promises nothing committed.
func TestReleaseReportsALostCommitAsUnknown(t *testing.T) {
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
	err = manager.Release(t.Context(), handle)
	if !killer.fired.Load() {
		t.Fatal("precondition: the release's COMMIT was never sent")
	}
	if !errors.Is(err, lease.ErrCommitUnknown) {
		t.Fatalf("release with a lost COMMIT = %v, want ErrCommitUnknown", err)
	}
	if errors.Is(err, lease.ErrSelfFenced) || errors.Is(err, lease.ErrStale) {
		t.Fatalf("release with a lost COMMIT = %v must not promise an outcome", err)
	}
}
