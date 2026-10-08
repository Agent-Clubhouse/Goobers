package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/claimability"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
)

// A successful provider page is pending selection evidence, not proof that
// shared/local claims or blocked-item rules would admit execution. candidates
// retains a bounded prefix of the matched items (identity and provider claim
// marker only) for in-memory claimability classification; it is never
// exported.
type backlogPollObservation struct {
	observedAt time.Time
	count      int
	complete   bool
	failed     bool
	candidates []claimability.Candidate
	truncated  bool
}
type backlogObservationReader interface{ backlogObservation() backlogPollObservation }

func (b *backlogCounter) retainBacklogObservation(observation backlogPollObservation, count int, err error) {
	observation.observedAt = time.Now().UTC()
	observation.count, observation.failed = count, err != nil
	if err != nil {
		observation.count, observation.complete, observation.candidates, observation.truncated = 0, false, nil, false
	}
	b.mu.Lock()
	b.observation = observation
	b.mu.Unlock()
}
func (b *backlogCounter) backlogObservation() backlogPollObservation {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observation
}
func admittedBacklogObservers(entries []localscheduler.WorkflowEntry) map[localscheduler.WorkflowIdentity]backlogObservationReader {
	result := map[localscheduler.WorkflowIdentity]backlogObservationReader{}
	if len(entries) > 1000 {
		return result
	}
	for _, entry := range entries {
		if entry.DisabledReason != "" {
			continue
		}
		source, _ := entry.BacklogCounter.(backlogObservationReader)
		if source == nil && entry.BacklogCounter == nil {
			source, _ = entry.ScheduleDemandCounter.(backlogObservationReader)
			if source == nil {
				source, _ = entry.RefillDemandCounter.(backlogObservationReader)
			}
			if source == nil {
				continue
			}
		}
		result[localscheduler.WorkflowIdentity{Gaggle: entry.Gaggle, Workflow: entry.Workflow}] = source
	}
	return result
}

type backlogAge struct {
	source   backlogObservationReader
	since    time.Time
	observed time.Time
}
type backlogHealthSampler struct {
	ages   map[localscheduler.WorkflowIdentity]backlogAge
	claims map[backlogObservationReader]backlogClaimCache
	probe  backlogClaimProbe
}

// backlogAggregate is one gaggle's covered evidence across its sources.
type backlogAggregate struct {
	matched, count, claimable     int
	incomplete, unheld            bool
	attention, claimableAttention bool
	observed                      time.Time
}

func withBacklogHealth(setup *schedulerSetup, health fleetHealthSample) fleetHealthSample {
	observer := &backlogHealthSampler{ages: map[localscheduler.WorkflowIdentity]backlogAge{}, probe: daemonBacklogClaimProbe(setup)}
	return func(ctx context.Context, now time.Time) []telemetry.DiagnosticRecord {
		records := health(ctx, now)
		if setup.Interventions == nil {
			return records
		}
		sources := setup.Interventions.Snapshot().backlogObservers
		retained := map[localscheduler.WorkflowIdentity]backlogAge{}
		for _, record := range records {
			gaggle, _ := record.Attributes["gaggleId"].(string)
			if gaggle == "" {
				continue
			}
			observer.observe(ctx, record.Attributes, gaggle, now, setup.Config.Telemetry.Diagnostics.ProgressPeriod(), sources, retained)
		}
		observer.ages = retained
		observer.pruneClaims(sources)
		return records
	}
}
func (s *backlogHealthSampler) observe(ctx context.Context, attrs map[string]any, gaggle string, now time.Time, threshold time.Duration, sources map[localscheduler.WorkflowIdentity]backlogObservationReader, retained map[localscheduler.WorkflowIdentity]backlogAge) {
	var aggregate backlogAggregate
	paused := attrs["state"] == "paused" || attrs["reasonCode"] == "startup"
	progress, _ := time.Parse(time.RFC3339Nano, stringAttribute(attrs, "lastUsefulProgressAt"))
	for identity, source := range sources {
		if identity.Gaggle != gaggle {
			continue
		}
		aggregate.matched++
		if source == nil {
			aggregate.incomplete = true
			continue
		}
		s.observeSource(ctx, &aggregate, identity, source, backlogSourceSample{now: now, threshold: threshold, paused: paused, progress: progress}, retained)
	}
	if aggregate.matched == 0 {
		return
	}
	writeBacklogCondition(attrs, aggregate, paused)
}

type backlogSourceSample struct {
	now, progress time.Time
	threshold     time.Duration
	paused        bool
}

// observeSource folds one source into the gaggle aggregate. A source whose
// every pending item was verified held or waiting is not aging work: its age
// resets exactly like a pause, so intentional deferral never reads as a stall.
func (s *backlogHealthSampler) observeSource(ctx context.Context, aggregate *backlogAggregate, identity localscheduler.WorkflowIdentity, source backlogObservationReader, sample backlogSourceSample, retained map[localscheduler.WorkflowIdentity]backlogAge) {
	now := sample.now
	evidence := source.backlogObservation()
	fresh := !evidence.observedAt.IsZero() && !evidence.observedAt.After(now) && now.Sub(evidence.observedAt) <= time.Minute
	if !fresh || evidence.failed {
		aggregate.incomplete = true
		return
	}
	if aggregate.observed.IsZero() || evidence.observedAt.Before(aggregate.observed) {
		aggregate.observed = evidence.observedAt
	}
	// Workflow selectors can overlap; maximum is a conservative lower bound.
	aggregate.count = max(aggregate.count, evidence.count)
	aggregate.incomplete = aggregate.incomplete || !evidence.complete
	if evidence.count <= 0 || sample.paused {
		return
	}
	claim, known := s.claimability(ctx, source, evidence)
	known = known && claim.Observed <= evidence.count
	if known {
		aggregate.claimable = max(aggregate.claimable, claim.Available)
	}
	held := known && claim.Complete && claim.Available == 0 && claim.Observed == evidence.count
	aggregate.unheld = aggregate.unheld || !held
	if !evidence.complete || held {
		return
	}
	age := s.ages[identity]
	if age.source != source || age.since.IsZero() || evidence.observedAt.Before(age.observed) || evidence.observedAt.Sub(age.observed) > time.Minute {
		age = backlogAge{source: source, since: evidence.observedAt}
	}
	if sample.progress.After(age.since) && !sample.progress.After(now) {
		age.since = sample.progress
	}
	age.observed = evidence.observedAt
	retained[identity] = age
	if now.Sub(age.since) >= sample.threshold {
		aggregate.attention = true
		aggregate.claimableAttention = aggregate.claimableAttention || known && claim.Available > 0
	}
}

// writeBacklogCondition projects covered source evidence into the closed wire
// contract. Positive partial counts remain lower bounds, verified claimable
// counts are lower bounds of admissible work, and pauses never acquire an
// attention state or claimability evidence.
func writeBacklogCondition(attrs map[string]any, aggregate backlogAggregate, paused bool) {
	count, complete := aggregate.count, !aggregate.incomplete
	attrs["backlogState"], attrs["backlogReasonCode"], attrs["backlogCoverage"] = "unknown", "observation_incomplete", "partial"
	if !aggregate.observed.IsZero() {
		attrs["backlogObservedAt"] = aggregate.observed.Format(time.RFC3339Nano)
	}
	if complete {
		attrs["backlogCoverage"] = "complete"
	}
	if count > 0 || complete {
		attrs["backlogPendingCount"] = count
	}
	switch {
	case paused:
		attrs["backlogReasonCode"] = "operator_paused"
	case count > 0 && aggregate.claimableAttention:
		attrs["backlogState"], attrs["backlogReasonCode"], attrs["backlogClaimableCount"] = "attention", "claimable_without_confirmed_progress", aggregate.claimable
	case count > 0 && aggregate.attention:
		attrs["backlogState"], attrs["backlogReasonCode"] = "attention", "pending_without_confirmed_progress"
	case count > 0 && aggregate.claimable > 0:
		attrs["backlogState"], attrs["backlogReasonCode"], attrs["backlogClaimableCount"] = "pending", "claimable_observed", aggregate.claimable
	case count > 0 && complete && !aggregate.unheld:
		attrs["backlogState"], attrs["backlogReasonCode"], attrs["backlogClaimableCount"] = "pending", "pending_held", 0
	case count > 0:
		attrs["backlogState"], attrs["backlogReasonCode"] = "pending", "claimability_unknown"
	case complete:
		attrs["backlogState"], attrs["backlogReasonCode"] = "empty", "no_pending_work"
	}
}
func stringAttribute(attrs map[string]any, key string) string {
	value, _ := attrs[key].(string)
	return value
}
