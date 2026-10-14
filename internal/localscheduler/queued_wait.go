package localscheduler

import "strings"

// WaitableForQueue reports an operating constraint that can hold an already
// accepted start without losing its pins or reserved run identity. It does not
// bypass admission: each later attempt must pass current policy again. Direct
// callers retain Transient's shorter capacity-only retry contract.
func (e *TriggerRejectedError) WaitableForQueue() bool {
	if e == nil {
		return false
	}
	if e.Transient() {
		return true
	}
	switch e.Reason {
	case ReasonBudget, ReasonDailyBudget, ReasonOpenPRCap:
		return true
	}
	for _, prefix := range []string{ReasonProviderQuota, ReasonDisabled, ReasonScheduleBackoff} {
		if e.Reason == prefix || strings.HasPrefix(e.Reason, prefix+": ") {
			return true
		}
	}
	// Missing capabilities, unavailable pinned placement, provider-auth failures
	// and unknown refusal reasons remain definitive; never infer permission from
	// wording that merely resembles one of the known operating constraints.
	return false
}
