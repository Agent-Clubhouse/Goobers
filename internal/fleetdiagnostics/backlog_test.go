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

func TestOfflineBacklogRejectsPrivateAndInvalidEvidence(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	attrs := map[string]any{"backlogState": "pending", "backlogReasonCode": "claimability_unknown", "backlogCoverage": "partial", "backlogPendingCount": 1, "backlogObservedAt": now.Format(time.RFC3339Nano), "prompt": "private"}
	if got, err := DecodeBacklogHealth(attrs, now); err != nil || got == nil || got.PendingCount == nil || *got.PendingCount != 1 {
		t.Fatalf("valid evidence: %v %v", got, err)
	}
	attrs["backlogPrivateIssue"] = "private"
	if _, err := DecodeBacklogHealth(attrs, now); err == nil {
		t.Fatal("unexpected backlog payload accepted")
	}
	delete(attrs, "backlogPrivateIssue")
	attrs["backlogPendingCount"] = 0
	if _, err := DecodeBacklogHealth(attrs, now); err == nil {
		t.Fatal("partial zero accepted")
	}
	if _, err := DecodeBacklogHealth(attrs, time.Time{}); err == nil {
		t.Fatal("missing observation time accepted")
	}
}

func TestBacklogClaimableEvidenceIsBoundToItsReason(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	evidence := func(state, reason, coverage string, pending, claimable any) map[string]any {
		attrs := map[string]any{"backlogState": state, "backlogReasonCode": reason, "backlogCoverage": coverage, "backlogPendingCount": pending, "backlogObservedAt": now.Format(time.RFC3339Nano)}
		if claimable != nil {
			attrs["backlogClaimableCount"] = claimable
		}
		return attrs
	}
	for name, attrs := range map[string]map[string]any{
		"observed":  evidence("pending", "claimable_observed", "partial", 3, 1),
		"attention": evidence("attention", "claimable_without_confirmed_progress", "complete", 3, 3),
		"held":      evidence("pending", "pending_held", "complete", 2, 0),
	} {
		if _, err := DecodeBacklogHealth(attrs, now); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	for name, attrs := range map[string]map[string]any{
		"zero claimable":      evidence("pending", "claimable_observed", "complete", 3, 0),
		"missing claimable":   evidence("pending", "claimable_observed", "complete", 3, nil),
		"exceeds pending":     evidence("pending", "claimable_observed", "complete", 1, 2),
		"attention observed":  evidence("attention", "claimable_observed", "complete", 3, 1),
		"partial held":        evidence("pending", "pending_held", "partial", 2, 0),
		"positive held":       evidence("pending", "pending_held", "complete", 2, 1),
		"unknown with count":  evidence("pending", "claimability_unknown", "complete", 2, 1),
		"pending stall count": evidence("attention", "pending_without_confirmed_progress", "complete", 2, 0),
	} {
		if _, err := DecodeBacklogHealth(attrs, now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	h, err := DecodeBacklogHealth(evidence("pending", "claimable_observed", "complete", 3, 2), now)
	if err != nil {
		t.Fatal(err)
	}
	if got := backlogReport(h, true, now.Add(time.Minute+time.Nanosecond)); got.ClaimableCount != nil || h.ClaimableCount == nil {
		t.Fatal("stale claimable evidence survived expiry")
	}
}
