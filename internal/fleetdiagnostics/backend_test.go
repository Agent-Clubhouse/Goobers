package fleetdiagnostics

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func heartbeatFields(now time.Time) map[string]any {
	return map[string]any{"schemaVersion": int64(1), "organization": "company-a", "environment": "production", "deploymentId": "deployment", "instanceId": "instance", "gaggleId": "gaggle", "ownerRef": "team-a", "component": "daemon", "bootId": "boot-1", "bootStartedAt": testTime.Add(-time.Minute).Format(time.RFC3339Nano), "sequence": int64(1), "observedAt": now.Format(time.RFC3339Nano), "windowStart": testTime.Add(-time.Minute).Format(time.RFC3339Nano), "windowCoverage": "complete", "state": "idle", "reasonCode": "no_eligible_work", "eligibleCount": int64(0), "version": "v0.5.0", "channel": "stable"}
}
func backendFixture(t testing.TB, now *time.Time) *Backend {
	t.Helper()
	b, err := New(func() time.Time { return *now }, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	policy := Tenant{Organization: "company-a", Owners: map[string]string{"team-a": "company-a/oncall"}, Catalogue: Catalogue{Source: "company-approved", ObservedAt: *now, Releases: map[string]string{"stable": "v0.5.0", "preview": "v0.5.0-rc.1"}}}
	if err := b.SetTenant("tenant-a", policy); err != nil {
		t.Fatal(err)
	}
	e := Enrollment{Identity: Identity{Organization: "company-a", Environment: "production", DeploymentID: "deployment", InstanceID: "instance", GaggleID: "gaggle", OwnerRef: "team-a", Component: "daemon"}, HeartbeatInterval: 30 * time.Second, MissedIntervals: 2}
	if err := b.Enroll("tenant-a", e); err != nil {
		t.Fatal(err)
	}
	return b
}
func oneReport(t *testing.T, b *Backend) Report {
	t.Helper()
	r, err := b.Reports("tenant-a")
	if err != nil || len(r) != 1 {
		t.Fatalf("reports %v %v", r, err)
	}
	return r[0]
}
func ingest(t *testing.T, b *Backend, name string, a map[string]any) {
	t.Helper()
	ok, err := b.Ingest("tenant-a", name, a)
	if !ok || err != nil {
		t.Fatalf("ingest=%v %v", ok, err)
	}
}
func TestHeartbeatBoundaryDedupAndRecovery(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	a := heartbeatFields(now)
	ingest(t, b, HeartbeatEvent, a)
	if r := oneReport(t, b); r.State != "idle" || r.OwnerRoute != "company-a/oncall" {
		t.Fatal(r)
	}
	now = now.Add(60*time.Second - time.Nanosecond)
	if r := oneReport(t, b); r.Liveness != "live" {
		t.Fatal(r)
	}
	if ok, err := b.Ingest("tenant-a", HeartbeatEvent, a); ok || err != nil {
		t.Fatalf("duplicate accepted %v %v", ok, err)
	}
	now = now.Add(time.Nanosecond)
	if r := oneReport(t, b); r.Liveness != "unreachable" || r.Reason != "missing_heartbeat" {
		t.Fatal(r)
	}
	a["sequence"] = int64(2)
	a["observedAt"] = now.Format(time.RFC3339Nano)
	a["state"] = "productive"
	a["reasonCode"] = "progress_observed"
	ingest(t, b, HeartbeatEvent, a)
	r := oneReport(t, b)
	if r.State != "productive" || r.Liveness != "live" || len(r.Transitions) < 3 {
		t.Fatal(r)
	}
}
func TestHealthTruthAndCollectorFailure(t *testing.T) {
	for _, state := range []string{"productive", "idle", "paused", "waiting", "backoff", "stalled", "unknown"} {
		t.Run(state, func(t *testing.T) {
			now := testTime
			b := backendFixture(t, &now)
			a := heartbeatFields(now)
			a["state"] = state
			ingest(t, b, HeartbeatEvent, a)
			if r := oneReport(t, b); r.State != state {
				t.Fatal(r)
			}
		})
	}
	now := testTime
	b := backendFixture(t, &now)
	a := heartbeatFields(now)
	a["state"] = "stalled"
	a["windowCoverage"] = "partial"
	ingest(t, b, HeartbeatEvent, a)
	if r := oneReport(t, b); r.State != "unknown" {
		t.Fatal(r)
	}
	p := b.tenants["tenant-a"]
	p.CollectorUnavailable = true
	if err := b.SetTenant("tenant-a", p); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if r := oneReport(t, b); r.Liveness != "unobserved" || r.Reason != "collector_unavailable" {
		t.Fatal(r)
	}
}
func TestHeartbeatClockAndOldBoot(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	a := heartbeatFields(now.Add(6 * time.Second))
	if ok, err := b.Ingest("tenant-a", HeartbeatEvent, a); ok || err == nil {
		t.Fatal("future skew accepted")
	}
	a = heartbeatFields(now)
	ingest(t, b, HeartbeatEvent, a)
	now = now.Add(time.Second)
	fresh := heartbeatFields(now)
	fresh["bootId"] = "boot-2"
	fresh["bootStartedAt"] = now.Format(time.RFC3339Nano)
	fresh["windowStart"] = now.Format(time.RFC3339Nano)
	ingest(t, b, HeartbeatEvent, fresh)
	if ok, err := b.Ingest("tenant-a", HeartbeatEvent, a); ok || err != nil {
		t.Fatalf("old boot replay accepted %v %v", ok, err)
	}
	if len(b.entries["tenant-a"][Key{"deployment", "instance", "gaggle"}].transitions) > MaxTransitions {
		t.Fatal("unbounded transitions")
	}
}
func featureFields(now time.Time) map[string]any {
	a := heartbeatFields(now)
	for _, k := range []string{"state", "reasonCode", "eligibleCount", "version", "channel"} {
		delete(a, k)
	}
	a["featureId"] = "runner.local"
	a["configured"] = true
	a["count"] = int64(0)
	a["windowEnd"] = now.Format(time.RFC3339Nano)
	return a
}
func TestFeaturesCoverageDedupAndRestart(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	ingest(t, b, HeartbeatEvent, heartbeatFields(now))
	a := featureFields(now)
	ingest(t, b, FeatureEvent, a)
	if u := oneReport(t, b).Features[0]; u.State != "unused" || u.Count == nil || *u.Count != 0 {
		t.Fatal(u)
	}
	a["sequence"] = int64(2)
	a["windowCoverage"] = "partial"
	ingest(t, b, FeatureEvent, a)
	if u := oneReport(t, b).Features[0]; u.State != "unknown" || u.Count != nil {
		t.Fatal(u)
	}
	a["sequence"] = int64(3)
	a["count"] = int64(5)
	ingest(t, b, FeatureEvent, a)
	if ok, err := b.Ingest("tenant-a", FeatureEvent, a); ok || err != nil {
		t.Fatal("duplicate feature accepted")
	}
	if u := oneReport(t, b).Features[0]; u.State != "used" || *u.Count != 5 {
		t.Fatal(u)
	}
	now = now.Add(time.Second)
	h := heartbeatFields(now)
	h["bootId"] = "boot-2"
	h["bootStartedAt"] = now.Format(time.RFC3339Nano)
	h["windowStart"] = now.Format(time.RFC3339Nano)
	ingest(t, b, HeartbeatEvent, h)
	if u := oneReport(t, b).Features[0]; u.Count != nil || u.State != "unknown" || u.Configured != nil {
		t.Fatal(u)
	}
	if ok, err := b.Ingest("tenant-a", FeatureEvent, a); ok || err == nil {
		t.Fatal("retired boot feature accepted")
	}
}
func TestTenantIsolationAndBounds(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	if err := b.SetTenant("tenant-b", Tenant{Organization: "company-b", Owners: map[string]string{"team-a": "company-b/oncall"}}); err != nil {
		t.Fatal(err)
	}
	e := b.entries["tenant-a"][Key{"deployment", "instance", "gaggle"}].enrollment
	e.Organization = "company-b"
	if err := b.Enroll("tenant-b", e); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.Ingest("tenant-b", HeartbeatEvent, heartbeatFields(now)); ok || err == nil {
		t.Fatal("cross-company event accepted")
	}
	ingest(t, b, HeartbeatEvent, heartbeatFields(now))
	reports, err := b.Reports("tenant-b")
	if err != nil || len(reports) != 1 || reports[0].Liveness != "unobserved" || reports[0].OwnerRoute != "company-b/oncall" {
		t.Fatalf("mixed tenants: %v %v", reports, err)
	}
	if _, err := b.Reports("unknown"); err == nil {
		t.Fatal("unauthorized query")
	}
	for i := 2; i < MaxInventory; i++ {
		e.DeploymentID = fmt.Sprint(i)
		if err := b.Enroll("tenant-b", e); err != nil {
			t.Fatal(err)
		}
	}
	e.DeploymentID = "overflow"
	if err := b.Enroll("tenant-b", e); err == nil {
		t.Fatal("unbounded inventory")
	}
	b.Remove("tenant-b", Key{"2", "instance", "gaggle"})
	if err := b.Enroll("tenant-b", e); err != nil {
		t.Fatal("remove did not free bound")
	}
}
func TestClosedContractRejectsPrivateAndMalformedFields(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(a map[string]any) { a["prompt"] = "private secret payload" }, func(a map[string]any) { a["schemaVersion"] = int64(2) }, func(a map[string]any) { a["state"] = "crashed" }, func(a map[string]any) { a["sequence"] = 1.5 }, func(a map[string]any) { a["eligibleCount"] = int64(-1) }, func(a map[string]any) { a["ownerRef"] = strings.Repeat("x", 257) }, func(a map[string]any) { a["observedAt"] = "not a timestamp" },
	} {
		a := heartbeatFields(testTime)
		mutate(a)
		if _, err := DecodeHeartbeat(a); err == nil {
			t.Fatal("invalid record accepted")
		}
	}
	a := featureFields(testTime)
	a["featureId"] = "capability.arbitrary-private-name"
	if _, err := DecodeFeatureUsage(a); err == nil {
		t.Fatal("unbounded feature accepted")
	}
}

func BenchmarkFleetHeartbeat(b *testing.B) {
	now := testTime
	backend := backendFixture(b, &now)
	attrs := heartbeatFields(now)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		attrs["sequence"] = int64(i + 1)
		if _, err := backend.Ingest("tenant-a", HeartbeatEvent, attrs); err != nil {
			b.Fatal(err)
		}
	}
}

func TestFeatureCounterRegressionAndStaleWindow(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	h := heartbeatFields(now)
	ingest(t, b, HeartbeatEvent, h)
	a := featureFields(now)
	a["count"] = int64(3)
	ingest(t, b, FeatureEvent, a)
	a["sequence"] = int64(2)
	a["count"] = int64(2)
	if ok, err := b.Ingest("tenant-a", FeatureEvent, a); ok || err == nil {
		t.Fatal("absolute counter regressed")
	}
	now = now.Add(66 * time.Second)
	h["sequence"] = int64(2)
	h["observedAt"] = now.Format(time.RFC3339Nano)
	ingest(t, b, HeartbeatEvent, h)
	if u := oneReport(t, b).Features[0]; u.State != "unknown" || u.Count != nil {
		t.Fatalf("old window counted as current: %+v", u)
	}
}
func TestUnobservedInventoryAndComponentIsolation(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	if r := oneReport(t, b); r.Liveness != "unobserved" || r.Features[0].Configured != nil {
		t.Fatal(r)
	}
	now = now.Add(time.Minute)
	if r := oneReport(t, b); r.Liveness != "unreachable" {
		t.Fatal(r)
	}
	a := heartbeatFields(now)
	a["component"] = "worker"
	if ok, err := b.Ingest("tenant-a", HeartbeatEvent, a); ok || err == nil {
		t.Fatal("different component overwrote daemon")
	}
}
