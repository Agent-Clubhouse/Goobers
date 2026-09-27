package fleetdiagnostics

import (
	"testing"
	"time"
)

func workerTestAttributes(state string) map[string]any {
	a := heartbeatFields(testTime)
	a["workerObservation"], a["workerCoverage"], a["workerObservedAt"] = state, "engine_workflow_activity_queue", testTime.Format(time.RFC3339Nano)
	if state == "recent_poller" {
		a["missingWorkerCount"] = int64(0)
	}
	if state == "no_recent_poller" {
		a["missingWorkerCount"] = int64(1)
	}
	return a
}
func TestWorkerHealthClosedContract(t *testing.T) {
	for _, state := range []string{"unknown", "not_required", "recent_poller", "no_recent_poller"} {
		a := workerTestAttributes(state)
		h, err := DecodeHeartbeat(a)
		if err != nil || h.Worker == nil || h.Worker.Observation != state {
			t.Fatalf("%s: %+v %v", state, h, err)
		}
		w, err := DecodeWorkerHealth(a, testTime)
		if err != nil || w == nil || w.Observation != state {
			t.Fatal(w, err)
		}
	}
	for _, mutate := range []func(map[string]any){
		func(a map[string]any) { a["workerObservation"] = "crashed" },
		func(a map[string]any) { a["workerCoverage"] = "all_workers" },
		func(a map[string]any) { a["workerObservedAt"] = testTime.Add(time.Second).Format(time.RFC3339Nano) },
		func(a map[string]any) { a["missingWorkerCount"] = int64(0) },
		func(a map[string]any) { a["workerObservedAt"] = testTime.Add(-time.Minute).Format(time.RFC3339Nano) },
		func(a map[string]any) { a["workerPrivate"] = "private" },
		func(a map[string]any) { delete(a, "workerObservedAt") },
	} {
		a := workerTestAttributes("no_recent_poller")
		mutate(a)
		if _, err := DecodeHeartbeat(a); err == nil {
			t.Fatal("invalid worker heartbeat accepted", a)
		}
		if _, err := DecodeWorkerHealth(a, testTime); err == nil {
			t.Fatal("invalid offline worker accepted", a)
		}
	}
	a := workerTestAttributes("unknown")
	a["missingWorkerCount"] = int64(0)
	if _, err := DecodeHeartbeat(a); err == nil {
		t.Fatal("unknown became known zero")
	}
}
func TestWorkerReportStaleEvidenceAndOwnership(t *testing.T) {
	one := int64(1)
	source := &WorkerHealth{Observation: "no_recent_poller", Coverage: "engine_workflow_activity_queue", ObservedAt: testTime, MissingCount: &one}
	live := workerReport(source, true)
	*live.MissingCount = 0
	if *source.MissingCount != 1 {
		t.Fatal("report mutated stored evidence")
	}
	stale := workerReport(source, false)
	if stale.Observation != "unknown" || stale.MissingCount != nil || source.Observation != "no_recent_poller" {
		t.Fatal(stale)
	}
}

func TestWorkerHealthBackendRecoveryAndExpiry(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	a := workerTestAttributes("no_recent_poller")
	a["state"], a["reasonCode"] = "waiting", "worker_unavailable"
	ingest(t, b, HeartbeatEvent, a)
	if r := oneReport(t, b); r.Worker == nil || r.Worker.Observation != "no_recent_poller" {
		t.Fatal(r)
	}
	now = now.Add(time.Second)
	a = workerTestAttributes("recent_poller")
	a["observedAt"], a["workerObservedAt"], a["sequence"] = now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), int64(2)
	ingest(t, b, HeartbeatEvent, a)
	if r := oneReport(t, b); r.Worker == nil || r.Worker.Observation != "recent_poller" || r.Worker.MissingCount == nil || *r.Worker.MissingCount != 0 {
		t.Fatal(r)
	}
	now = now.Add(time.Minute)
	if r := oneReport(t, b); r.Worker == nil || r.Worker.Observation != "unknown" || r.Worker.MissingCount != nil {
		t.Fatal("expired observation claimed live polling", r)
	}
}
