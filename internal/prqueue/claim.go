package prqueue

import "time"

// ClaimObservation describes this instance's ledger, not a global claim
// oracle. Provider labels are independent observations and grant no authority.
type ClaimObservation struct {
	State              string     `json:"state"`
	OwnerRunID         string     `json:"ownerRunId,omitempty"`
	ExpiresAt          *time.Time `json:"expiresAt,omitempty"`
	ProviderClaimLabel bool       `json:"providerClaimLabel"`
	Comparison         string     `json:"comparison"`
	NextStep           string     `json:"nextStep"`
}

// ObserveClaim compares a ledger snapshot with the independently observed
// provider label. It never grants permission to mutate either source.
func ObserveClaim(known bool, owner, currentRun string, expiry, observedAt time.Time, providerLabel bool) ClaimObservation {
	result := ClaimObservation{State: "unknown", ProviderClaimLabel: providerLabel, Comparison: "unavailable", NextStep: "Restore access to the claim ledger before inferring claim availability."}
	if !known {
		return result
	}
	result.State = "unclaimed"
	result.Comparison = "no-local-lease-or-provider-label"
	result.NextStep = "Re-evaluate eligibility before attempting an atomic claim."
	if owner != "" {
		result.OwnerRunID = owner
		result.ExpiresAt = &expiry
		result.State = "expired"
		if expiry.After(observedAt) {
			result.State = "held-by-other-run"
			result.NextStep = "Inspect the owning run; do not steal its live lease."
			if owner == currentRun {
				result.State = "held-by-this-run"
			}
		}
	}
	active := result.State == "held-by-other-run" || result.State == "held-by-this-run"
	switch {
	case providerLabel && active:
		result.Comparison = "local-lease-and-provider-label"
	case providerLabel:
		result.Comparison = "provider-label-without-live-local-lease"
		result.NextStep = "Check other instances and provider claim ownership before considering reconciliation; the local ledger is not global authority."
	case active:
		result.Comparison = "local-lease-without-provider-label"
		result.NextStep = "Inspect the owning run and whether this workflow mirrors PR claims to labels; label absence does not invalidate the lease."
	}
	return result
}
