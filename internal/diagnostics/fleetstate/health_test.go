package fleetstate

import (
	"testing"
	"time"
)

func TestClassifyHealthBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	zero, one, two := 0, 1, 2
	base := Observation{ObservedAt: now, EligibleCount: &zero, InflightCount: &zero, Complete: true}
	tests := []struct {
		name   string
		change func(*Observation)
		want   Verdict
	}{
		{"healthy idle", func(o *Observation) {}, Verdict{"idle", "no_eligible_work"}},
		{"paused", func(o *Observation) { o.Paused = true; o.EligibleCount = &one }, Verdict{"paused", "operator_paused"}},
		{"user gate", func(o *Observation) { o.WaitingOnUser = true }, Verdict{"waiting", "waiting_for_gate"}},
		{"retry", func(o *Observation) { o.BackoffUntil = now.Add(time.Minute) }, Verdict{"backoff", "retry_backoff"}},
		{"missing worker", func(o *Observation) { o.MissingWorkerCount = &one }, Verdict{"waiting", "worker_unavailable"}},
		{"partial", func(o *Observation) { o.Complete = false }, Verdict{"unknown", "observation_incomplete"}},
		{"eligibility unknown", func(o *Observation) { o.EligibleCount = nil }, Verdict{"unknown", "work_eligibility_unknown"}},
		{"stalled exact boundary", func(o *Observation) { o.EligibleCount = &one; o.OldestEligibleAt = now.Add(-30 * time.Minute) }, Verdict{"stalled", "no_progress"}},
		{"before stall boundary", func(o *Observation) {
			o.EligibleCount = &one
			o.OldestEligibleAt = now.Add(-30*time.Minute + time.Nanosecond)
		}, Verdict{"waiting", "eligible_within_threshold"}},
		{"progress clears stall", func(o *Observation) {
			o.EligibleCount = &one
			o.OldestEligibleAt = now.Add(-time.Hour)
			o.LastUsefulProgressAt = now.Add(-time.Minute)
		}, Verdict{"waiting", "eligible_within_threshold"}},
		{"valid long stage", func(o *Observation) {
			o.InflightCount = &one
			o.ActiveDeadline = now.Add(time.Hour)
			o.LastUsefulProgressAt = now.Add(-time.Hour)
		}, Verdict{"waiting", "stage_within_deadline"}},
		{"productive", func(o *Observation) { o.InflightCount = &one; o.LastUsefulProgressAt = now.Add(-time.Second) }, Verdict{"productive", "progress_observed"}},
		{"unknown stage deadline", func(o *Observation) { o.InflightCount = &one }, Verdict{"unknown", "stage_deadline_unknown"}},
		{"no work loops", func(o *Observation) { o.NoWorkCount = &two }, Verdict{"idle", "repeated_no_work"}},
		{"future observation", func(o *Observation) { o.ObservedAt = now.Add(time.Second) }, Verdict{"unknown", "clock_skew"}},
		{"stale observation", func(o *Observation) { o.ObservedAt = now.Add(-time.Minute - time.Nanosecond) }, Verdict{"unknown", "observation_stale"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			tc.change(&o)
			if got := Classify(o, now, 30*time.Minute, time.Minute); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}
