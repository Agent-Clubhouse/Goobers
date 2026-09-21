package main

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prqueue"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

type fleetTestReader struct {
	now      time.Time
	eligible bool
	stale    bool
	runs     []readservice.RunSummary
	paused   bool
}

func fleetTestEnvelope() readservice.ReadStateEnvelope {
	return readservice.ReadStateEnvelope{ReadState: &readmodel.ReadState{Completeness: readmodel.CompletenessComplete}}
}
func (r *fleetTestReader) Gaggles(context.Context, readservice.PageRequest) (readservice.GagglePage, error) {
	return readservice.GagglePage{ReadStateEnvelope: fleetTestEnvelope(), Items: []readservice.Gaggle{{Name: "alpha", Enabled: !r.paused}}}, nil
}
func (r *fleetTestReader) Workflows(context.Context, string, readservice.PageRequest) (readservice.WorkflowPage, error) {
	return readservice.WorkflowPage{ReadStateEnvelope: fleetTestEnvelope(), Items: []readservice.WorkflowSummary{{Identity: readservice.WorkflowReference{Name: "work"}, Enabled: true}}}, nil
}
func (r *fleetTestReader) ListStatusRuns(context.Context, readservice.StatusRunOptions) ([]readservice.RunSummary, error) {
	return r.runs, nil
}
func (r *fleetTestReader) QueueEligibility(context.Context, string, string) (readservice.QueueEligibilityView, error) {
	at := r.now
	if r.stale {
		at = at.Add(-time.Hour)
	}
	return readservice.QueueEligibilityView{ReadStateEnvelope: fleetTestEnvelope(), Report: &prqueue.Report{ObservedAt: at, CompleteSnapshot: true, Items: []prqueue.Item{{Eligible: r.eligible, Claim: prqueue.ClaimObservation{State: "unclaimed"}}}}}, nil
}
func TestFleetHealthSamplerStateAndOwnerRouting(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	reader := &fleetTestReader{now: now}
	observer := &fleetHealthObserver{reader: reader, config: &instance.DiagnosticsConfig{Organization: "company", OwnerRef: "team:default", GaggleOwners: map[string]string{"alpha": "team:alpha"}, ProgressTimeout: "1m"}, instanceID: "deployment-a", bootID: "boot-a", startedAt: now, eligibleSince: map[string]time.Time{}}
	sample := func() map[string]any {
		t.Helper()
		records := observer.sample(context.Background(), reader.now)
		if len(records) != 2 {
			t.Fatal(len(records))
		}
		return records[1].Attributes
	}
	attrs := sample()
	if attrs["state"] != "idle" || attrs["ownerRef"] != "team:alpha" || attrs["organization"] != "company" {
		t.Fatal(attrs)
	}
	reader.eligible = true
	attrs = sample()
	if attrs["state"] != "waiting" {
		t.Fatal(attrs)
	}
	reader.now = now.Add(time.Minute)
	attrs = sample()
	if attrs["state"] != "stalled" {
		t.Fatal(attrs)
	}
	reader.eligible = false
	attrs = sample()
	if attrs["state"] != "idle" {
		t.Fatal(attrs)
	}
	reader.stale = true
	attrs = sample()
	if attrs["state"] != "unknown" || attrs["eligibleCount"] != nil {
		t.Fatal(attrs)
	}
	reader.paused = true
	attrs = sample()
	if attrs["state"] != "paused" {
		t.Fatal(attrs)
	}
}
func TestFleetHealthNoWorkDoesNotInventUsefulProgress(t *testing.T) {
	now := time.Now().UTC()
	reader := &fleetTestReader{now: now, eligible: true}
	observer := &fleetHealthObserver{reader: reader, config: &instance.DiagnosticsConfig{ProgressTimeout: "1m"}, startedAt: now.Add(-time.Hour), eligibleSince: map[string]time.Time{"alpha": now.Add(-time.Hour)}}
	finished := now
	reader.runs = []readservice.RunSummary{{Gaggle: "alpha", StartedAt: now.Add(-time.Second), FinishedAt: &finished, Terminal: true, NoWork: true}}
	attrs := observer.sample(context.Background(), now)[1].Attributes
	if attrs["state"] != "stalled" || attrs["lastUsefulProgressAt"] != nil {
		t.Fatal(attrs)
	}
	reader.runs[0].NoWork = false
	reader.runs[0].Phase = journal.PhaseCompleted
	attrs = observer.sample(context.Background(), now)[1].Attributes
	if attrs["state"] == "stalled" || attrs["lastUsefulProgressAt"] == nil {
		t.Fatal(attrs)
	}
}

func TestFleetAvailableCountRequiresClaimEvidence(t *testing.T) {
	for _, tc := range []struct {
		state string
		label bool
		count int
		known bool
	}{
		{"unknown", false, 0, false}, {"unclaimed", true, 0, false},
		{"held-by-other-run", false, 0, true}, {"held-by-this-run", true, 0, true},
		{"unclaimed", false, 1, true}, {"expired", false, 1, true},
	} {
		count, known := fleetAvailableCount([]prqueue.Item{{Eligible: true, Claim: prqueue.ClaimObservation{State: tc.state, ProviderClaimLabel: tc.label}}})
		if count != tc.count || known != tc.known {
			t.Fatalf("%+v got count=%d known=%v", tc, count, known)
		}
	}
}

func TestFleetHealthStartupTransitions(t *testing.T) {
	now := time.Now().UTC()
	ready := false
	observer := &fleetHealthObserver{reader: &fleetTestReader{now: now}, config: &instance.DiagnosticsConfig{}, instanceID: "instance", bootID: "boot", startedAt: now, eligibleSince: map[string]time.Time{}, ready: func() bool { return ready }}
	records := observer.sample(context.Background(), now)
	for _, record := range records {
		if record.Attributes["state"] != "waiting" || record.Attributes["reasonCode"] != "startup" {
			t.Fatalf("startup claimed settled health: %+v", record)
		}
	}
	ready = true
	records = observer.sample(context.Background(), now)
	if records[1].Attributes["state"] != "idle" {
		t.Fatalf("readiness transition not observed: %+v", records[1])
	}
}

func TestDaemonHealthStopsWhenStartupReturnsEarly(t *testing.T) {
	log := openTestInstanceLog(t)
	setup := &schedulerSetup{Config: &instance.Config{}, InstanceLog: log}
	stop := startDaemonHealth(context.Background(), t.TempDir(), nil, setup, nil, &fleetTestReader{now: time.Now()}, func() bool { return false })
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostic workers survived startup exit")
	}
	if len(readServiceHealthEvents(t, log)) != 1 {
		t.Fatal("startup evidence missing")
	}
}

type fleetUnavailableReader struct {
	fleetTestReader
	queried bool
}

func (*fleetUnavailableReader) FleetDiagnosticsAvailable(context.Context) bool { return false }
func (r *fleetUnavailableReader) Gaggles(context.Context, readservice.PageRequest) (readservice.GagglePage, error) {
	r.queried = true
	return readservice.GagglePage{}, nil
}
func TestFleetUnavailableProjectionAvoidsHistoricalReads(t *testing.T) {
	reader := &fleetUnavailableReader{}
	observer := &fleetHealthObserver{reader: reader, config: &instance.DiagnosticsConfig{}, startedAt: time.Now()}
	records := observer.sample(context.Background(), time.Now())
	if reader.queried || len(records) != 1 || records[0].Attributes["windowCoverage"] != "unknown" {
		t.Fatalf("unavailable index fell back to inventory: queried=%v records=%+v", reader.queried, records)
	}
}

func TestFleetIntentionalWaitResetsStallWindow(t *testing.T) {
	now := time.Now().UTC()
	reader := &fleetTestReader{now: now, eligible: true}
	observer := &fleetHealthObserver{reader: reader, config: &instance.DiagnosticsConfig{ProgressTimeout: "1m"}, startedAt: now, eligibleSince: map[string]time.Time{}}
	observer.sample(context.Background(), now)
	reader.paused = true
	reader.now = now.Add(2 * time.Minute)
	if got := observer.sample(context.Background(), reader.now)[1].Attributes["state"]; got != "paused" {
		t.Fatal(got)
	}
	reader.paused = false
	reader.now = now.Add(3 * time.Minute)
	if got := observer.sample(context.Background(), reader.now)[1].Attributes["state"]; got != "waiting" {
		t.Fatalf("paused time became a stall: %v", got)
	}
	reader.stale = true
	reader.now = now.Add(5 * time.Minute)
	observer.sample(context.Background(), reader.now)
	reader.stale = false
	reader.now = now.Add(6 * time.Minute)
	if got := observer.sample(context.Background(), reader.now)[1].Attributes["state"]; got != "waiting" {
		t.Fatalf("unknown interval became a stall: %v", got)
	}
}
