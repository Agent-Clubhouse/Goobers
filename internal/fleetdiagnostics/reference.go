package fleetdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/goobers/goobers/internal/telemetry"
)

// ReferenceFixture executes real loopback OTLP/gRPC exports from two synthetic
// companies with colliding deployment IDs. It prints tenant-scoped queries,
// then advances a fake backend clock to demonstrate absence and collector gaps.
// This opt-in example binds loopback only, uses fake credentials, and is not a
// production server. It neither files issues nor contacts any owner/upstream.
func ReferenceFixture(ctx context.Context, out io.Writer) error {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	backend, err := New(func() time.Time { return now }, 5*time.Second)
	if err != nil {
		return err
	}
	if err := enrollReferenceCompanies(backend, now); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	receiver, err := NewReceiver(backend, referenceAuthorization)
	if err != nil {
		_ = listener.Close()
		return err
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(MaxRequestBytes))
	collectorlogpb.RegisterLogsServiceServer(server, receiver)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	if err := exportReferenceCompanies(ctx, listener.Addr().String(), now); err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := writeReferenceQueries(encoder, backend, "initial"); err != nil {
		return err
	}
	now = now.Add(60 * time.Second)
	if err := writeReferenceQueries(encoder, backend, "two_missed_intervals"); err != nil {
		return err
	}
	policy := referenceTenant("company-a", now)
	policy.CollectorUnavailable = true
	if err := backend.SetTenant("company-a", policy); err != nil {
		return err
	}
	if err := writeReferenceQueries(encoder, backend, "independent_collector_failure_evidence"); err != nil {
		return err
	}
	backend.Remove("company-a", Key{"same-deployment", "same-instance", "two"})
	return writeReferenceQueries(encoder, backend, "retired_inventory")
}
func referenceAuthorization(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) != 1 {
		return "", errors.New("missing credential")
	}
	for _, tenant := range []string{"company-a", "company-b"} {
		if values[0] == "Bearer synthetic-"+tenant {
			return tenant, nil
		}
	}
	return "", errors.New("unknown credential")
}
func referenceTenant(tenant string, now time.Time) Tenant {
	return Tenant{Organization: tenant, Owners: map[string]string{"team-one": tenant + "/oncall-one", "team-two": tenant + "/oncall-two"}, Catalogue: Catalogue{Source: tenant + "/approved-releases", ObservedAt: now, Releases: map[string]string{"stable": "v0.5.0", "preview": "v0.5.0-rc.1"}}}
}
func enrollReferenceCompanies(b *Backend, now time.Time) error {
	for _, tenant := range []string{"company-a", "company-b"} {
		if err := b.SetTenant(tenant, referenceTenant(tenant, now)); err != nil {
			return err
		}
		for _, gaggle := range []string{"one", "two"} {
			identity := Identity{Organization: tenant, Environment: "production", DeploymentID: "same-deployment", InstanceID: "same-instance", GaggleID: gaggle, OwnerRef: "team-" + gaggle, Component: "daemon"}
			if err := b.Enroll(tenant, Enrollment{Identity: identity, HeartbeatInterval: 30 * time.Second, MissedIntervals: 2}); err != nil {
				return err
			}
		}
	}
	return nil
}
func referenceFields(tenant, gaggle string, now time.Time) map[string]any {
	return map[string]any{"schemaVersion": int64(1), "organization": tenant, "environment": "production", "deploymentId": "same-deployment", "instanceId": "same-instance", "gaggleId": gaggle, "ownerRef": "team-" + gaggle, "component": "daemon", "bootId": "synthetic-boot", "bootStartedAt": now.Add(-time.Minute).Format(time.RFC3339Nano), "sequence": int64(1), "observedAt": now.Format(time.RFC3339Nano), "windowStart": now.Add(-time.Minute).Format(time.RFC3339Nano), "windowCoverage": "complete"}
}
func exportReferenceCompanies(ctx context.Context, endpoint string, now time.Time) error {
	states := []struct{ tenant, gaggle, state, reason, version string }{{"company-a", "one", "idle", "no_eligible_work", "v0.5.0"}, {"company-a", "two", "waiting", "worker_unavailable", "v0.4.1"}, {"company-b", "one", "productive", "progress_observed", "v0.5.0"}, {"company-b", "two", "paused", "operator_paused", "v0.5.0"}}
	for _, sample := range states {
		attrs := referenceFields(sample.tenant, sample.gaggle, now)
		attrs["state"] = sample.state
		attrs["reasonCode"] = sample.reason
		attrs["version"] = sample.version
		attrs["channel"] = "stable"
		if err := exportReference(ctx, endpoint, sample.tenant, HeartbeatEvent, attrs); err != nil {
			return err
		}
		usage := referenceFields(sample.tenant, sample.gaggle, now)
		usage["featureId"] = "runner.local"
		usage["configured"] = true
		usage["count"] = int64(0)
		usage["windowEnd"] = now.Format(time.RFC3339Nano)
		if sample.state == "productive" {
			usage["count"] = int64(3)
		}
		if err := exportReference(ctx, endpoint, sample.tenant, FeatureEvent, usage); err != nil {
			return err
		}
	}
	return nil
}
func exportReference(ctx context.Context, endpoint string, tenant, name string, attrs map[string]any) error {
	exporter, err := telemetry.NewDiagnosticExporter(telemetry.Config{OTLPEndpoint: endpoint, OTLPInsecure: true, OTLPHeaders: map[string]string{"authorization": "Bearer synthetic-" + tenant}})
	if err != nil {
		return err
	}
	exporter.Emit(telemetry.DiagnosticRecord{Time: time.Now(), Name: "goobers.service.health", Attributes: map[string]any{"schemaVersion": 1, "instanceId": "same-instance"}})
	exporter.Emit(telemetry.DiagnosticRecord{Time: time.Now(), Name: name, Attributes: attrs})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := exporter.Shutdown(ctx); err != nil {
		return err
	}
	if stats := exporter.Stats(); stats.Accepted != 2 || stats.Delivered != 2 || stats.Dropped != 0 || stats.Failures != 0 {
		return errors.New("reference diagnostic delivery failed")
	}
	return nil
}
func writeReferenceQueries(encoder *json.Encoder, b *Backend, phase string) error {
	for _, tenant := range []string{"company-a", "company-b"} {
		reports, err := b.Reports(tenant)
		if err != nil {
			return err
		}
		for _, report := range reports {
			if !strings.HasPrefix(report.OwnerRoute, tenant+"/") {
				return fmt.Errorf("owner route escaped tenant")
			}
		}
		if err := encoder.Encode(struct {
			Phase   string   `json:"phase"`
			Tenant  string   `json:"tenant"`
			Reports []Report `json:"reports"`
		}{phase, tenant, reports}); err != nil {
			return err
		}
	}
	return nil
}
