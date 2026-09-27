package main

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestBacklogPendingAgeCoveragePauseReloadAndProgress(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	counter := &backlogCounter{observation: backlogPollObservation{observedAt: now, count: 2, complete: true}}
	identity := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "issues"}
	sources := map[localscheduler.WorkflowIdentity]backlogObservationReader{identity: counter}
	sampler := &backlogHealthSampler{ages: map[localscheduler.WorkflowIdentity]backlogAge{}}
	observe := func(at time.Time, state string, progress time.Time) map[string]any {
		attrs := map[string]any{"state": state, "lastUsefulProgressAt": progress.Format(time.RFC3339Nano)}
		retained := map[localscheduler.WorkflowIdentity]backlogAge{}
		sampler.observe(attrs, "g", at, time.Minute, sources, retained)
		sampler.ages = retained
		return attrs
	}
	if got := observe(now, "unknown", time.Time{}); got["backlogState"] != "pending" || got["backlogPendingCount"] != 2 {
		t.Fatal(got)
	}
	counter.observation.observedAt = now.Add(time.Minute)
	if got := observe(now.Add(time.Minute), "unknown", time.Time{}); got["backlogState"] != "attention" {
		t.Fatal(got)
	}
	if got := observe(now.Add(time.Minute), "unknown", now.Add(time.Minute)); got["backlogState"] != "pending" {
		t.Fatal("useful progress did not reset age", got)
	}
	observe(now.Add(time.Minute), "paused", time.Time{})
	if len(sampler.ages) != 0 {
		t.Fatal("pause retained age")
	}
	counter.observation.complete = false
	if got := observe(now.Add(time.Minute), "unknown", time.Time{}); got["backlogCoverage"] != "partial" || got["backlogState"] != "pending" {
		t.Fatal(got)
	}
	if len(sampler.ages) != 0 {
		t.Fatal("partial coverage retained age")
	}
	counter.observation.count = 0
	if got := observe(now.Add(time.Minute), "unknown", time.Time{}); got["backlogPendingCount"] != nil {
		t.Fatal("partial zero", got)
	}
	counter.observation.complete = true
	if got := observe(now.Add(time.Minute), "unknown", time.Time{}); got["backlogState"] != "empty" {
		t.Fatal(got)
	}
	if got := observe(now.Add(3*time.Minute), "unknown", time.Time{}); got["backlogPendingCount"] != nil {
		t.Fatal("stale count", got)
	}
	counter.observation = backlogPollObservation{observedAt: now.Add(3 * time.Minute), count: 1, complete: true}
	observe(now.Add(3*time.Minute), "unknown", time.Time{})
	replacement := &backlogCounter{observation: backlogPollObservation{observedAt: now.Add(4 * time.Minute), count: 1, complete: true}}
	sources[identity] = replacement
	if got := observe(now.Add(4*time.Minute), "unknown", time.Time{}); got["backlogState"] != "pending" {
		t.Fatal("new generation inherited age", got)
	}
}

func TestBacklogFailedPollDoesNotBecomeEmpty(t *testing.T) {
	counter := &backlogCounter{}
	counter.retainBacklogObservation(backlogPollObservation{complete: true}, 0, errors.New("provider unavailable"))
	got := counter.backlogObservation()
	if !got.failed || got.complete || got.observedAt.IsZero() {
		t.Fatal(got)
	}
}

func TestBacklogObservationStrictWireAndOverlappingSelectors(t *testing.T) {
	now := time.Now().UTC()
	source := &backlogCounter{observation: backlogPollObservation{observedAt: now, count: 2, complete: true}}
	sources := map[localscheduler.WorkflowIdentity]backlogObservationReader{{Gaggle: "g", Workflow: "a"}: source, {Gaggle: "g", Workflow: "b"}: source}
	sampler := &backlogHealthSampler{ages: map[localscheduler.WorkflowIdentity]backlogAge{}}
	attrs := map[string]any{"schemaVersion": 1, "deploymentId": "d", "instanceId": "i", "gaggleId": "g", "component": "daemon", "bootId": "b", "bootStartedAt": now.Format(time.RFC3339Nano), "sequence": 1, "observedAt": now.Format(time.RFC3339Nano), "windowStart": now.Format(time.RFC3339Nano), "windowCoverage": "partial", "state": "unknown", "reasonCode": "work_eligibility_unknown"}
	sampler.observe(attrs, "g", now, time.Minute, sources, map[localscheduler.WorkflowIdentity]backlogAge{})
	h, err := fleetdiagnostics.DecodeHeartbeat(attrs)
	if err != nil || h.Backlog == nil || h.Backlog.PendingCount == nil || *h.Backlog.PendingCount != 2 {
		t.Fatalf("%+v %v", h.Backlog, err)
	}
	if h.EligibleCount != nil {
		t.Fatal("pending work asserted claimability")
	}
}
