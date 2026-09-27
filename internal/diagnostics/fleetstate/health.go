// Package fleetstate classifies operational observations without inferring
// provider root causes or treating absence of evidence as healthy inactivity.
package fleetstate

import "time"

// Observation contains measured scheduler facts. Nil counts mean unobserved.
// Times originate in retained evidence, not the time this snapshot was read.
type Observation struct {
	StorageFailure       bool
	CleanupFailure       bool
	AdmissionSaturated   bool
	ProviderThrottled    bool
	ObservedAt           time.Time
	LastUsefulProgressAt time.Time
	OldestEligibleAt     time.Time
	ActiveDeadline       time.Time
	EligibleCount        *int
	InflightCount        *int
	MissingWorkerCount   *int
	Paused               bool
	WaitingOnUser        bool
	BackoffUntil         time.Time
	NoWorkCount          *int
	Complete             bool
}

// Verdict reports an observed condition, never an unsupported root cause.
type Verdict struct{ State, ReasonCode string }

// Classify keeps intentional waits and valid long stages out of stall alerts.
// A stall requires positive eligible-work evidence and an elapsed threshold.
func Classify(o Observation, now time.Time, progressTimeout, maxAge time.Duration) Verdict {
	if verdict, invalid := observationProblem(o, now, progressTimeout, maxAge); invalid {
		return verdict
	}
	if o.Paused {
		return Verdict{"paused", "operator_paused"}
	}
	if o.WaitingOnUser {
		return Verdict{"waiting", "waiting_for_gate"}
	}
	if o.BackoffUntil.After(now) {
		if o.ProviderThrottled {
			return Verdict{"backoff", "provider_throttled"}
		}
		return Verdict{"backoff", "retry_backoff"}
	}
	if verdict, blocked := operationalBlock(o); blocked {
		return verdict
	}
	if o.MissingWorkerCount != nil && *o.MissingWorkerCount > 0 {
		return Verdict{"waiting", "worker_unavailable"}
	}
	if !o.Complete {
		return Verdict{"unknown", "observation_incomplete"}
	}
	if verdict, active := activeStageVerdict(o, now, progressTimeout); active {
		return verdict
	}
	if o.EligibleCount == nil || o.InflightCount == nil {
		return Verdict{"unknown", "work_eligibility_unknown"}
	}
	if *o.EligibleCount == 0 && *o.InflightCount == 0 {
		if o.NoWorkCount != nil && *o.NoWorkCount > 1 {
			return Verdict{"idle", "repeated_no_work"}
		}
		return Verdict{"idle", "no_eligible_work"}
	}
	if *o.EligibleCount > 0 {
		reference := o.OldestEligibleAt
		if o.LastUsefulProgressAt.After(reference) {
			reference = o.LastUsefulProgressAt
		}
		if reference.IsZero() {
			return Verdict{"unknown", "progress_window_unknown"}
		}
		if now.Sub(reference) >= progressTimeout {
			return Verdict{"stalled", "no_progress"}
		}
		return Verdict{"waiting", "eligible_within_threshold"}
	}
	return Verdict{"unknown", "progress_unconfirmed"}
}

func observationProblem(o Observation, now time.Time, progressTimeout, maxAge time.Duration) (Verdict, bool) {
	if now.IsZero() || o.ObservedAt.IsZero() || progressTimeout <= 0 || maxAge <= 0 {
		return Verdict{"unknown", "observation_unavailable"}, true
	}
	if o.ObservedAt.After(now) || o.LastUsefulProgressAt.After(now) || o.OldestEligibleAt.After(now) {
		return Verdict{"unknown", "clock_skew"}, true
	}
	if now.Sub(o.ObservedAt) > maxAge {
		return Verdict{"unknown", "observation_stale"}, true
	}
	for _, count := range []*int{o.EligibleCount, o.InflightCount, o.MissingWorkerCount, o.NoWorkCount} {
		if count != nil && *count < 0 {
			return Verdict{"unknown", "observation_unavailable"}, true
		}
	}
	return Verdict{}, false
}

func activeStageVerdict(o Observation, now time.Time, progressTimeout time.Duration) (Verdict, bool) {
	if o.InflightCount != nil && *o.InflightCount > 0 {
		if o.ActiveDeadline.After(now) {
			return Verdict{"waiting", "stage_within_deadline"}, true
		}
		if !o.LastUsefulProgressAt.IsZero() && now.Sub(o.LastUsefulProgressAt) < progressTimeout {
			return Verdict{"productive", "progress_observed"}, true
		}
		// An unknown deadline is not evidence that a valid quiet stage is stuck.
		if o.ActiveDeadline.IsZero() {
			return Verdict{"unknown", "stage_deadline_unknown"}, true
		}
	}
	return Verdict{}, false
}

func operationalBlock(o Observation) (Verdict, bool) {
	switch {
	case o.StorageFailure:
		return Verdict{"waiting", "storage_failure"}, true
	case o.CleanupFailure:
		return Verdict{"waiting", "cleanup_failure"}, true
	case o.AdmissionSaturated:
		return Verdict{"waiting", "admission_saturated"}, true
	default:
		return Verdict{}, false
	}
}
