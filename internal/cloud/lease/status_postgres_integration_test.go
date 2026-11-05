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
	current, found := findCurrent(status, handle.InstanceID, handle.Name)
	history, historyFound := findHistory(status, handle.InstanceID, handle.Name, handle.Epoch)
	if !found || current.Holder != handle.Holder ||
		current.Epoch != handle.Epoch || current.Expired ||
		current.ExpiresAt == nil || !historyFound || history.ReleasedAt != nil {
		t.Fatalf("active lease status = %+v", status)
	}
	if err := manager.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	status, err = lease.Status(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	current, found = findCurrent(status, handle.InstanceID, handle.Name)
	history, historyFound = findHistory(status, handle.InstanceID, handle.Name, handle.Epoch)
	if !found || current.Holder != "" ||
		current.Epoch != handle.Epoch+1 || !current.Expired ||
		!historyFound || history.ReleasedAt == nil ||
		history.ReleaseKind != "voluntary" {
		t.Fatalf("released lease status = %+v", status)
	}
}

func findCurrent(status lease.StatusReport, instanceID, name string) (lease.Current, bool) {
	for _, current := range status.Leases {
		if current.InstanceID == instanceID && current.Name == name {
			return current, true
		}
	}
	return lease.Current{}, false
}

func findHistory(status lease.StatusReport, instanceID, name string, epoch int64) (lease.HistoryEntry, bool) {
	for _, history := range status.History {
		if history.InstanceID == instanceID && history.Name == name && history.Epoch == epoch {
			return history, true
		}
	}
	return lease.HistoryEntry{}, false
}
