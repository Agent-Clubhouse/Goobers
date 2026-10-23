//go:build cloudintegration

package acceptance

import (
	"testing"
	"time"
)

func TestChildAcceptanceIsNotGenericStartRecovery(t *testing.T) {
	store, owner := childTestStore(t)
	child, _, err := store.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDispatch(t.Context(), child.AcceptanceID); err != nil {
		t.Fatalf("premise: child launch moves acceptance to dispatching: %v", err)
	}
	if _, err := owner.Exec(t.Context(), `UPDATE control.acceptance SET dispatch_started_at=clock_timestamp()-interval '5 minutes' WHERE id=$1`, child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	ordinary, _, err := store.Accept(t.Context(), "ordinary", "actor", []byte("pinned"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDispatch(t.Context(), ordinary.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `UPDATE control.acceptance SET dispatch_started_at=clock_timestamp()-interval '5 minutes' WHERE id=$1`, ordinary.ID); err != nil {
		t.Fatal(err)
	}
	retries, err := store.reserveStartRetries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(retries) != 1 || retries[0].ID != ordinary.ID {
		t.Fatalf("recovery must select only ordinary acceptance: %+v", retries)
	}
	api, err := New(childRolePool(t, owner, "goobers_api"), store.instanceID)
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := api.StartStatusPage(t.Context(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].AcceptanceID != ordinary.ID {
		t.Fatalf("status must show only ordinary acceptance: %+v", statuses)
	}
	var count int
	var retryAt *time.Time
	if err := owner.QueryRow(t.Context(), "SELECT start_retry_count,start_retry_at FROM control.acceptance WHERE id=$1", child.AcceptanceID).Scan(&count, &retryAt); err != nil {
		t.Fatal(err)
	}
	if count != 0 || retryAt != nil {
		t.Fatalf("recovery mutated child retry custody: count=%d at=%v", count, retryAt)
	}
}
