//go:build cloudintegration

package acceptance

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// A fresh engine bind IS the child's dispatch, in the same transaction: the
// acceptance leaves 'accepted' (so no start loop ever considers it) and the
// child leaves 'queued' (#8085 d). Finalize's heal for older control planes
// would otherwise mask a bind that stopped dispatching (#8183 review P2-1).
func TestFreshEngineBindDispatchesTheChild(t *testing.T) {
	store, owner := childTestStore(t)
	child, _, err := store.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := owner.QueryRow(t.Context(), `SELECT state FROM control.acceptance WHERE instance_id=$1 AND id=$2`, store.instanceID, child.AcceptanceID).Scan(&state); err != nil || state != "accepted" {
		t.Fatalf("premise: acceptance state before bind = %q, %v", state, err)
	}
	if _, err := store.BindChildEngineStart(t.Context(), child.Identity, engineStartFor(child, "run-1")); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(t.Context(), `SELECT state FROM control.acceptance WHERE instance_id=$1 AND id=$2`, store.instanceID, child.AcceptanceID).Scan(&state); err != nil || state != "dispatched" {
		t.Fatalf("acceptance after a fresh bind = %q, %v; want dispatched (the bind is the dispatch)", state, err)
	}
	got, err := store.GetChild(t.Context(), child.Identity)
	if err != nil || got.State != triggerqueue.ChildRunning {
		t.Fatalf("child after a fresh bind = %q, %v; want running", got.State, err)
	}
}

// The bind's dispatch is strict: a fresh bind whose acceptance is no longer
// 'accepted' (a start loop or another path moved it) fails as a whole, leaving
// no engine start and the child queued, rather than binding a child its
// acceptance does not agree was dispatched by this bind.
func TestFreshEngineBindRefusesAnAcceptanceNoLongerAccepted(t *testing.T) {
	store, owner := childTestStore(t)
	child, _, err := store.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `UPDATE control.acceptance SET state='dispatching' WHERE instance_id=$1 AND id=$2`, store.instanceID, child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindChildEngineStart(t.Context(), child.Identity, engineStartFor(child, "run-1")); err == nil {
		t.Fatal("a fresh bind dispatched over an acceptance that was no longer accepted")
	}
	if _, err := store.ChildEngineStart(t.Context(), child.Identity); err == nil {
		t.Fatal("a refused bind left an engine start behind")
	}
	if got, err := store.GetChild(t.Context(), child.Identity); err != nil || got.State != triggerqueue.ChildQueued {
		t.Fatalf("child after a refused bind = %q, %v; want queued", got.State, err)
	}
}
