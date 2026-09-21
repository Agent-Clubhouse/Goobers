package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
)

// A successful provider page is pending selection evidence, not proof that
// shared/local claims or blocked-item rules would admit execution.
type backlogPollObservation struct {
	observedAt time.Time
	count      int
	complete   bool
	failed     bool
}
type backlogObservationReader interface{ backlogObservation() backlogPollObservation }

func (b *backlogCounter) retainBacklogObservation(observation backlogPollObservation, count int, err error) {
	observation.observedAt = time.Now().UTC()
	observation.count, observation.failed = count, err != nil
	if err != nil {
		observation.count, observation.complete = 0, false
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
	ages map[localscheduler.WorkflowIdentity]backlogAge
}

func withBacklogHealth(setup *schedulerSetup, health fleetHealthSample) fleetHealthSample {
	observer := &backlogHealthSampler{ages: map[localscheduler.WorkflowIdentity]backlogAge{}}
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
			observer.observe(record.Attributes, gaggle, now, setup.Config.Telemetry.Diagnostics.ProgressPeriod(), sources, retained)
		}
		observer.ages = retained
		return records
	}
}
func (s *backlogHealthSampler) observe(attrs map[string]any, gaggle string, now time.Time, threshold time.Duration, sources map[localscheduler.WorkflowIdentity]backlogObservationReader, retained map[localscheduler.WorkflowIdentity]backlogAge) {
	count, matched := 0, 0
	complete, attention := true, false
	var observed time.Time
	paused := attrs["state"] == "paused" || attrs["reasonCode"] == "startup"
	progress, _ := time.Parse(time.RFC3339Nano, stringAttribute(attrs, "lastUsefulProgressAt"))
	for identity, source := range sources {
		if identity.Gaggle != gaggle {
			continue
		}
		matched++
		if source == nil {
			complete = false
			continue
		}
		evidence := source.backlogObservation()
		fresh := !evidence.observedAt.IsZero() && !evidence.observedAt.After(now) && now.Sub(evidence.observedAt) <= time.Minute
		if !fresh || evidence.failed {
			complete = false
			continue
		}
		if observed.IsZero() || evidence.observedAt.Before(observed) {
			observed = evidence.observedAt
		}
		// Workflow selectors can overlap; maximum is a conservative lower bound.
		if evidence.count > count {
			count = evidence.count
		}
		complete = complete && evidence.complete
		if paused || !evidence.complete || evidence.count <= 0 {
			continue
		}
		age := s.ages[identity]
		if age.source != source || age.since.IsZero() || evidence.observedAt.Before(age.observed) || evidence.observedAt.Sub(age.observed) > time.Minute {
			age = backlogAge{source: source, since: evidence.observedAt}
		}
		if progress.After(age.since) && !progress.After(now) {
			age.since = progress
		}
		age.observed = evidence.observedAt
		retained[identity] = age
		attention = attention || now.Sub(age.since) >= threshold
	}
	if matched == 0 {
		return
	}
	writeBacklogCondition(attrs, count, complete, attention, paused, observed)
}

// writeBacklogCondition projects covered source evidence into the closed wire
// contract. Positive partial counts remain lower bounds, and pauses never
// acquire an attention state.
func writeBacklogCondition(attrs map[string]any, count int, complete, attention, paused bool, observed time.Time) {
	attrs["backlogState"], attrs["backlogReasonCode"], attrs["backlogCoverage"] = "unknown", "observation_incomplete", "partial"
	if !observed.IsZero() {
		attrs["backlogObservedAt"] = observed.Format(time.RFC3339Nano)
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
	case count > 0 && attention:
		attrs["backlogState"], attrs["backlogReasonCode"] = "attention", "pending_without_confirmed_progress"
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
