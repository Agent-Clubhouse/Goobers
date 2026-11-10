//go:build cloudintegration

package sharedclaims

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgroles"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/providers"
)

type reconcileWriter struct {
	cursor string
	next   string
	err    error
	calls  int
}

func (w *reconcileWriter) ReconcileSharedVisibility(_ context.Context, _ providers.RepositoryRef, cursor string, _ int) (string, error) {
	w.calls++
	w.cursor = cursor
	return w.next, w.err
}

func TestRepositoryRegistryRetriesAcrossEpochsAsController(t *testing.T) {
	owner := pgtest.Pool(t, "sharedregistry")
	pool := pgtest.ControllerPool(t, owner)
	var role string
	if err := pool.QueryRow(t.Context(), `SELECT current_user`).Scan(&role); err != nil || role != pgroles.Controller {
		t.Fatalf("role=%s, %v", role, err)
	}
	manager := pgtest.Manager(t, pool, "first", 0)
	handle := pgtest.Acquire(t, manager)
	registry := Registry{Pool: pool, InstanceID: pgtest.InstanceID, Fence: manager, Owner: func() *lease.Handle { return handle }}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "service"}
	ctx := t.Context()
	for range 2 {
		if err := registry.Register(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	writer := &reconcileWriter{next: "42"}
	if err := registry.Reconcile(ctx, handle, writer); err != nil || writer.cursor != "" {
		t.Fatalf("initial sweep=%+v, %v", writer, err)
	}
	if err := registry.Register(ctx, repo); err != nil {
		t.Fatal(err)
	}
	writer.err = errors.New("provider unavailable")
	if err := registry.Reconcile(ctx, handle, writer); err == nil || writer.cursor != "42" {
		t.Fatalf("retry custody=%+v, %v", writer, err)
	}
	nextManager := pgtest.Manager(t, pool, "second", 0)
	nextHandle := pgtest.Takeover(t, owner, nextManager)
	calls := writer.calls
	if err := registry.Reconcile(ctx, handle, writer); err == nil || writer.calls != calls {
		t.Fatalf("old leader wrote labels: calls=%d err=%v", writer.calls, err)
	}
	registry.Fence = nextManager
	registry.Owner = func() *lease.Handle { return nextHandle }
	writer.err = nil
	writer.next = ""
	if err := registry.Reconcile(ctx, nextHandle, writer); err != nil || writer.cursor != "42" {
		t.Fatalf("takeover cursor=%+v, %v", writer, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM control.shared_claim_repository WHERE instance_id=$1`, pgtest.InstanceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("durable registry count=%d, %v", count, err)
	}
}
