//go:build cloudintegration

package childengine

import "testing"

// Slot-limit hooks must use the same durable scheduler state as production
// control. This needs no parent/worker, so a child-protocol failure cannot mask
// the reload/failover wiring regression.
func TestQualificationSlotLimitUsesPostgresState(t *testing.T) {
	h := newQualificationHarness(t)
	assertCapacity := func(limit int) {
		t.Helper()
		releases := make([]func(), 0, limit)
		defer func() {
			for _, release := range releases {
				release()
			}
		}()
		for _, runID := range []string{"slot-first", "slot-second"}[:limit] {
			release, ok, reason := h.epoch.scheduler.ReserveContinuation(runID, "example", "default-implement")
			if !ok {
				t.Fatalf("reserve %s with limit %d: %s", runID, limit, reason)
			}
			releases = append(releases, release)
		}
		if release, ok, _ := h.epoch.scheduler.ReserveContinuation("slot-overflow", "example", "default-implement"); ok {
			release()
			t.Fatalf("admitted above slot limit %d", limit)
		}
	}
	assertPersisted := func() {
		t.Helper()
		var count int
		err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.scheduler_state
WHERE instance_id=$1 AND scope IN ('schedule-demand', 'trigger-evaluations')
AND key='snapshot' AND owner_epoch=$2`, testInstance, h.epoch.handle.Epoch).Scan(&count)
		if err != nil || count != 2 {
			t.Fatalf("current epoch persisted %d scheduler snapshots, want 2: %v", count, err)
		}
	}
	h.SetSlotLimit(2)
	assertCapacity(2)
	assertPersisted()
	oldEpoch := h.epoch.handle.Epoch
	h.FailoverControl()
	if h.epoch.handle.Epoch <= oldEpoch {
		t.Fatal("control failover did not advance the lease epoch")
	}
	h.SetSlotLimit(1)
	assertCapacity(1)
	assertPersisted()
}
