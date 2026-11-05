//go:build cloudintegration

package journalpg

import (
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
)

// hostOwner mimics control.Host.Owner: the live handle while the epoch runs,
// nil once the epoch ends (the host clears it when it cancels the epoch context),
// though the lease itself is still held until the host releases it.
type hostOwner struct {
	handle atomic.Pointer[lease.Handle]
}

func (o *hostOwner) get() *lease.Handle { return o.handle.Load() }

func instanceSeqOwner(t *testing.T, b *Backend) (token *string) {
	t.Helper()
	if err := b.config.Pool.QueryRow(t.Context(), `SELECT owner_token FROM journal.instance_seq WHERE instance_id=$1 AND stream=$2`,
		b.config.InstanceID, instanceStream).Scan(&token); err != nil {
		t.Fatal(err)
	}
	return token
}

// The retire must work after the epoch ended: the live owner is gone, but the
// lease the stream was opened under still fences it. It clears the owner token
// (so the next incarnation opens cleanly) and drops the in-memory entry.
func TestRetireAfterTheEpochEndsClearsTheTokenAndDropsTheEntry(t *testing.T) {
	pool := pgtest.Pool(t, "jpgretire")
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	var owner hostOwner
	owner.handle.Store(handle)
	backend, err := New(Config{Pool: pgtest.ControllerPool(t, pool), InstanceID: pgtest.InstanceID, Lease: manager, LiveOwner: owner.get})
	if err != nil {
		t.Fatal(err)
	}
	log, err := backend.OpenSchedulerLog(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if token := instanceSeqOwner(t, backend); token == nil {
		t.Fatal("the stream has no owner token after open")
	}
	backend.mu.Lock()
	open := len(backend.instances)
	backend.mu.Unlock()
	if open != 1 {
		t.Fatalf("instances after open = %d, want 1", open)
	}

	owner.handle.Store(nil) // the epoch ended; the lease is still held
	if err := log.Close(); err != nil {
		t.Fatalf("retire after the epoch ended = %v", err)
	}
	if token := instanceSeqOwner(t, backend); token != nil {
		t.Fatalf("owner token %q survived the retire", *token)
	}
	backend.mu.Lock()
	remaining := len(backend.instances)
	backend.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("instances after retire = %d, want the entry dropped", remaining)
	}
	// The next incarnation (same epoch lease, new open) is not refused.
	owner.handle.Store(handle)
	if _, err := backend.OpenSchedulerLog(t.Context(), nil); err != nil {
		t.Fatalf("reopen after retire = %v", err)
	}
}

// A retire that cannot be fenced (the lease was lost to a successor) still drops
// the entry: one entry per epoch must never accumulate.
func TestRetireThatLostItsLeaseStillDropsTheEntry(t *testing.T) {
	pool := pgtest.Pool(t, "jpgretire")
	oldManager := pgtest.ControllerManager(t, pool, "old", 0)
	old := pgtest.Acquire(t, oldManager)
	var owner hostOwner
	owner.handle.Store(old)
	backend, err := New(Config{Pool: pgtest.ControllerPool(t, pool), InstanceID: pgtest.InstanceID, Lease: oldManager, LiveOwner: owner.get})
	if err != nil {
		t.Fatal(err)
	}
	log, err := backend.OpenSchedulerLog(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Takeover(t, pool, pgtest.ControllerManager(t, pool, "new", 0))
	owner.handle.Store(nil)
	if err := log.Close(); err == nil {
		t.Fatal("a deposed leader's retire reported success")
	}
	backend.mu.Lock()
	remaining := len(backend.instances)
	backend.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("instances after a failed retire = %d, want 0 (no leak)", remaining)
	}
}
