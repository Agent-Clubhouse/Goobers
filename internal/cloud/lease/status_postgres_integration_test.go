//go:build cloudintegration

package lease_test

import (
	"testing"

	"github.com/goobers/goobers/internal/cloud/lease"
)

func TestLeaseStatusUsesDatabaseClockAndIncludesHistory(t *testing.T) {
	pool := testPool(t)
	manager := testManager(t, pool, "holder-a")
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	status, err := lease.Status(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Leases) != 1 || status.Leases[0].Holder != handle.Holder ||
		status.Leases[0].Epoch != handle.Epoch || status.Leases[0].Expired ||
		status.Leases[0].ExpiresAt == nil || len(status.History) != 1 ||
		status.History[0].ReleasedAt != nil {
		t.Fatalf("active lease status = %+v", status)
	}
	if err := manager.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	status, err = lease.Status(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Leases) != 1 || status.Leases[0].Holder != "" ||
		status.Leases[0].Epoch != handle.Epoch+1 || !status.Leases[0].Expired ||
		len(status.History) != 1 || status.History[0].ReleasedAt == nil ||
		status.History[0].ReleaseKind != "voluntary" {
		t.Fatalf("released lease status = %+v", status)
	}
}
