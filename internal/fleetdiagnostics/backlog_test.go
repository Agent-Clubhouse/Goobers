package fleetdiagnostics

import (
	"testing"
	"time"
)

func TestBacklogWireAndIndependentExpiry(t *testing.T) {
	now := time.Now().UTC()
	attrs := heartbeatFields(now)
	attrs["backlogState"], attrs["backlogReasonCode"], attrs["backlogCoverage"] = "attention", "pending_without_confirmed_progress", "complete"
	attrs["backlogPendingCount"], attrs["backlogObservedAt"] = 2, now.Format(time.RFC3339Nano)
	h, err := DecodeHeartbeat(attrs)
	if err != nil {
		t.Fatal(err)
	}
	if got := backlogReport(h.Backlog, true, now.Add(time.Minute)); got.State != "attention" {
		t.Fatal(got)
	}
	if got := backlogReport(h.Backlog, true, now.Add(time.Minute+time.Nanosecond)); got.State != "unknown" || got.PendingCount != nil {
		t.Fatal(got)
	}
	if h.Backlog.State != "attention" {
		t.Fatal("query mutated retained observation")
	}
	attrs["backlogState"], attrs["backlogReasonCode"], attrs["backlogPendingCount"], attrs["backlogCoverage"] = "empty", "no_pending_work", 0, "partial"
	if _, err := DecodeHeartbeat(attrs); err == nil {
		t.Fatal("partial empty accepted")
	}
	attrs["backlogCoverage"] = "complete"
	if _, err := DecodeHeartbeat(attrs); err != nil {
		t.Fatal(err)
	}
	attrs["backlogObservedAt"] = now.Add(time.Second).Format(time.RFC3339Nano)
	if _, err := DecodeHeartbeat(attrs); err == nil {
		t.Fatal("future evidence accepted")
	}
}
