package localscheduler

import "testing"

func TestAcceptedQueueWaitsWithoutChangingDirectRetryContract(t *testing.T) {
	for _, reason := range []string{ReasonBudget, ReasonDailyBudget, ReasonOpenPRCap, ReasonProviderQuota, ReasonProviderQuota + ": resumes at next window", ReasonDisabled, ReasonDisabled + ": workflow paused", ReasonScheduleBackoff, ReasonScheduleBackoff + ": resumes later"} {
		refusal := &TriggerRejectedError{Reason: reason}
		if !refusal.WaitableForQueue() || refusal.Transient() {
			t.Fatalf("wrong accepted/direct wait contracts: %q", reason)
		}
	}
	for _, reason := range []string{ReasonMaxParallel, ReasonInstanceMaxParallel, ReasonMemoryPressure + ": 90%"} {
		refusal := &TriggerRejectedError{Reason: reason}
		if !refusal.WaitableForQueue() || !refusal.Transient() {
			t.Fatalf("capacity contract changed: %q", reason)
		}
	}
	for _, reason := range []string{ReasonMissingCapability + ": git", ReasonHarnessUnavailable + ": missing", ReasonPlacementUnsatisfiable + ": no runner", ReasonProviderAuth + ": revoked", ReasonProviderQuota + "-unknown", ReasonDisabled + "-unknown", ReasonScheduleBackoff + "-unknown", "unknown", ""} {
		if (&TriggerRejectedError{Reason: reason}).WaitableForQueue() {
			t.Fatalf("definitive refusal converted to wait: %q", reason)
		}
	}
	var refusal *TriggerRejectedError
	if refusal.WaitableForQueue() {
		t.Fatal("nil refusal waited")
	}
}
