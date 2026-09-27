package main

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

func TestFleetRetryBackoffRequiresEveryActiveRunAndFreshTimer(t *testing.T) {
	now := time.Now().UTC()
	boot := now.Add(-time.Minute)
	base := readservice.RunSummary{Gaggle: "g", RetryBackoff: readmodel.RetryBackoffState{Waits: []readmodel.RetryBackoff{{Stage: "work", Driver: "local", ObservedAt: now, Deadline: now.Add(time.Minute)}}}}
	if got := fleetRetryBackoff([]readservice.RunSummary{base}, "g", now, boot); !got.Equal(now.Add(time.Minute)) {
		t.Fatal(got)
	}
	for _, mutate := range []func(*readservice.RunSummary){
		func(r *readservice.RunSummary) { r.ActiveStages = []readmodel.ActiveStage{{Name: "parallel"}} },
		func(r *readservice.RunSummary) { r.WaitingForGate = true },
		func(r *readservice.RunSummary) { r.RetryBackoff.Truncated = true },
		func(r *readservice.RunSummary) { r.RetryBackoff.Parallel = true },
		func(r *readservice.RunSummary) { r.RetryBackoff.Waits = nil },
		func(r *readservice.RunSummary) { r.Gaggle = "other" },
	} {
		run := base
		mutate(&run)
		if got := fleetRetryBackoff([]readservice.RunSummary{base, run}, "g", now, boot); !got.IsZero() {
			t.Fatal("unrelated work hidden by backoff", got)
		}
	}
	if got := fleetRetryBackoff([]readservice.RunSummary{base}, "g", now.Add(2*time.Minute), boot); !got.IsZero() {
		t.Fatal("expired timer remains waiting")
	}
	if got := fleetRetryBackoff([]readservice.RunSummary{base}, "g", now, now.Add(time.Second)); !got.IsZero() {
		t.Fatal("local timer survived process restart")
	}
	engine := base
	engine.RetryBackoff.Waits = append([]readmodel.RetryBackoff(nil), base.RetryBackoff.Waits...)
	engine.RetryBackoff.Waits[0].Driver = "engine"
	if got := fleetRetryBackoff([]readservice.RunSummary{engine}, "g", now, now.Add(time.Second)); got.IsZero() {
		t.Fatal("durable engine timer lost solely to daemon restart")
	}
	later := base
	later.RetryBackoff.Waits = append([]readmodel.RetryBackoff(nil), base.RetryBackoff.Waits...)
	later.RetryBackoff.Waits[0].Deadline = now.Add(2 * time.Minute)
	if got := fleetRetryBackoff([]readservice.RunSummary{base, later}, "g", now, boot); !got.Equal(base.RetryBackoff.Waits[0].Deadline) {
		t.Fatal("later timer masked earliest retry", got)
	}
}

func TestFleetRetryBackoffProductionSamplerExpiresAndRecovers(t *testing.T) {
	now := time.Now().UTC()
	reader := &fleetTestReader{now: now, eligible: true, runs: []readservice.RunSummary{{Gaggle: "alpha", RetryBackoff: readmodel.RetryBackoffState{Waits: []readmodel.RetryBackoff{{Stage: "work", Driver: "local", ObservedAt: now, Deadline: now.Add(time.Minute)}}}}}}
	observer := &fleetHealthObserver{reader: reader, config: &instance.DiagnosticsConfig{ProgressTimeout: "1m"}, startedAt: now.Add(-time.Hour), eligibleSince: map[string]time.Time{"alpha": now.Add(-time.Hour)}}
	attrs := observer.sample(context.Background(), now)[1].Attributes
	if attrs["state"] != "backoff" || attrs["reasonCode"] != "retry_backoff" {
		t.Fatalf("timer not consumed by sampler: %+v", attrs)
	}
	if _, exists := observer.eligibleSince["alpha"]; exists {
		t.Fatal("intentional wait accrued stall time")
	}
	reader.now = now.Add(time.Minute)
	attrs = observer.sample(context.Background(), reader.now)[1].Attributes
	if attrs["state"] == "backoff" || attrs["state"] == "stalled" {
		t.Fatalf("expired timer retained or immediately stalled: %+v", attrs)
	}
	reader.runs = nil
	reader.eligible = false
	attrs = observer.sample(context.Background(), reader.now)[1].Attributes
	if attrs["state"] != "idle" {
		t.Fatalf("recovery did not clear: %+v", attrs)
	}
}

func TestFleetRetryBackoffDoesNotInferIdleSiblingCoverage(t *testing.T) {
	now := time.Now().UTC()
	// Branch 1 is in a known timer. Branch 2 can be between stages or waiting
	// for admission, so neither branch currently has an ActiveStage entry.
	run := readservice.RunSummary{Gaggle: "alpha", RetryBackoff: readmodel.RetryBackoffState{Waits: []readmodel.RetryBackoff{{Stage: "branch-work", Branch: 1, Driver: "local", ObservedAt: now, Deadline: now.Add(time.Minute)}}}}
	if got := fleetRetryBackoff([]readservice.RunSummary{run}, "alpha", now, now.Add(-time.Hour)); !got.IsZero() {
		t.Fatalf("uncovered parallel sibling declared in backoff: %v", got)
	}
	reader := &fleetTestReader{now: now, eligible: true, runs: []readservice.RunSummary{run}}
	observer := &fleetHealthObserver{reader: reader, config: &instance.DiagnosticsConfig{ProgressTimeout: "1m"}, startedAt: now.Add(-time.Hour), eligibleSince: map[string]time.Time{}}
	attrs := observer.sample(context.Background(), now)[1].Attributes
	if attrs["state"] == "backoff" {
		t.Fatalf("sampler inferred whole-run branch coverage: %+v", attrs)
	}
}
