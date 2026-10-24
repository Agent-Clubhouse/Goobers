//go:build cloudintegration

package acceptance

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// Custody still owned by an earlier epoch is a distinct, retryable condition
// from both of its producers, never "cancelled", and adoption clears it.
func TestStaleChildEpochIsDistinctAndClearedByAdoption(t *testing.T) {
	store, pool := childTestStore(t)
	grant := reviewAuthority("parent-stale", "stage")
	if err := store.BindChildAuthority(t.Context(), grant, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.manager.Release(t.Context(), store.handle); err != nil {
		t.Fatal(err)
	}
	successor := successorStore(t, childRolePool(t, pool, "goobers_controller"))
	successor.BindChildBlobs(store.childBlobs)

	replacement := grant
	replacement.GrantID, replacement.AttemptID = "grant-2", "attempt-2"
	if err := successor.BindChildAuthority(t.Context(), replacement, grant.GrantID, time.Now()); !errors.Is(err, ErrStaleChildEpoch) {
		t.Fatalf("authority bind on unadopted custody = %v, want ErrStaleChildEpoch", err)
	}
	// A release on unadopted custody is retried, never read as "already
	// replaced", which a releasing worker would treat as done.
	if err := successor.RevokeChildAuthority(t.Context(), grant); !errors.Is(err, ErrStaleChildEpoch) {
		t.Fatalf("authority revoke on unadopted custody = %v, want ErrStaleChildEpoch", err)
	}
	request := childRequest("parent-stale", "stage", "call-1")
	if _, _, err := successor.AcceptChild(t.Context(), request, time.Now()); !errors.Is(err, ErrStaleChildEpoch) {
		t.Fatalf("child acceptance on unadopted custody = %v, want ErrStaleChildEpoch", err)
	}

	if adopted, err := successor.AdoptStaleChildCustody(t.Context()); err != nil || adopted == 0 {
		t.Fatalf("adoption = %d, %v", adopted, err)
	}
	if err := successor.BindChildAuthority(t.Context(), replacement, grant.GrantID, time.Now()); err != nil {
		t.Fatalf("authority bind after adoption: %v", err)
	}
	if _, _, err := successor.AcceptChild(t.Context(), request, time.Now()); err != nil {
		t.Fatalf("child acceptance after adoption: %v", err)
	}
}

// A parent cancelled on the current epoch refuses new authority permanently:
// ErrParentCancelled, never the retryable stale-epoch class.
func TestBindChildAuthorityOnACurrentEpochCancelledParentIsRefused(t *testing.T) {
	store, _ := childTestStore(t)
	grant := reviewAuthority("parent-cancelled", "stage")
	if err := store.FenceChildParent(t.Context(), grant.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.BindChildAuthority(t.Context(), grant, "", time.Now()); !errors.Is(err, triggerqueue.ErrParentCancelled) {
		t.Fatalf("bind on a current-epoch cancelled parent = %v, want ErrParentCancelled", err)
	}
}
