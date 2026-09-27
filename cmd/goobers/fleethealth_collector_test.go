package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
)

type fleetCollectorReader struct{ fleetTestReader }

func (r *fleetCollectorReader) Gaggles(context.Context, readservice.PageRequest) (readservice.GagglePage, error) {
	return readservice.GagglePage{ReadStateEnvelope: fleetTestEnvelope(), Items: []readservice.Gaggle{{Name: "alpha", Enabled: true}, {Name: "beta", Enabled: false}}}, nil
}

// Use the production sampler, exporter and authenticated backend together. Both
// companies deliberately collide on deployment and gaggle identifiers.
func TestFleetProducerThroughCollectorKeepsTenantHealthAndOwners(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	backend, err := fleetdiagnostics.New(func() time.Time { return now }, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := fleetdiagnostics.NewReceiver(backend, func(ctx context.Context) (string, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("authorization")
		if len(values) == 1 && (values[0] == "company-a" || values[0] == "company-b") {
			return values[0], nil
		}
		return "", errors.New("unknown company")
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(fleetdiagnostics.MaxRequestBytes))
	collectorlogpb.RegisterLogsServiceServer(server, receiver)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	observers := map[string]*fleetHealthObserver{}
	readers := map[string]*fleetCollectorReader{}
	for _, tenant := range []string{"company-a", "company-b"} {
		if err := backend.SetTenant(tenant, fleetdiagnostics.Tenant{Organization: tenant, Owners: map[string]string{"alpha-team": tenant + "/alpha-oncall", "beta-team": tenant + "/beta-oncall"}}); err != nil {
			t.Fatal(err)
		}
		for _, gaggle := range []string{"", "alpha", "beta"} {
			identity := fleetdiagnostics.Identity{Organization: tenant, DeploymentID: "same-deployment", InstanceID: "same-deployment", GaggleID: gaggle, Component: "daemon"}
			if err := backend.Enroll(tenant, fleetdiagnostics.Enrollment{Identity: identity, HeartbeatInterval: 30 * time.Second, MissedIntervals: 2}); err != nil {
				t.Fatal(err)
			}
		}
		reader := &fleetCollectorReader{fleetTestReader: fleetTestReader{now: now}}
		readers[tenant] = reader
		observers[tenant] = &fleetHealthObserver{reader: reader, config: &instance.DiagnosticsConfig{Organization: tenant, GaggleOwners: map[string]string{"alpha": "alpha-team", "beta": "beta-team"}, ProgressTimeout: "1m"}, instanceID: "same-deployment", bootID: tenant + "-boot", startedAt: now, eligibleSince: map[string]time.Time{}}
	}
	send := func(tenant string) {
		t.Helper()
		readers[tenant].now = now
		exporter, err := telemetry.NewDiagnosticExporter(telemetry.Config{OTLPEndpoint: listener.Addr().String(), OTLPInsecure: true, OTLPHeaders: map[string]string{"authorization": tenant}})
		if err != nil {
			t.Fatal(err)
		}
		records := observers[tenant].sample(context.Background(), now)
		accepted := exporter.EmitBatch(records)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := exporter.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		stats := exporter.Stats()
		if accepted != 3 || stats.Delivered != 3 || stats.Dropped != 0 {
			t.Fatalf("production wire rejected: accepted=%d stats=%+v", accepted, stats)
		}
	}
	check := func(tenant, alphaState string) {
		t.Helper()
		reports, err := backend.Reports(tenant)
		if err != nil || len(reports) != 3 {
			t.Fatalf("inventory %s: %+v %v", tenant, reports, err)
		}
		for _, report := range reports {
			if report.Organization != tenant {
				t.Fatalf("cross-company data: %+v", report)
			}
			switch report.GaggleID {
			case "alpha":
				if report.State != alphaState || report.OwnerRoute != tenant+"/alpha-oncall" {
					t.Fatalf("alpha %s: %+v", tenant, report)
				}
			case "beta":
				if report.State != "paused" || report.OwnerRoute != tenant+"/beta-oncall" {
					t.Fatalf("beta %s: %+v", tenant, report)
				}
			}
		}
	}
	send("company-a")
	send("company-b")
	check("company-a", "idle")
	check("company-b", "idle")
	readers["company-a"].eligible = true
	send("company-a")
	check("company-a", "waiting")
	check("company-b", "idle")
	now = now.Add(time.Minute)
	send("company-a")
	send("company-b")
	check("company-a", "stalled")
	check("company-b", "idle")
	readers["company-a"].eligible = false
	send("company-a")
	check("company-a", "idle")
	reports, err := backend.Reports("company-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range reports {
		if report.GaggleID == "alpha" && len(report.Transitions) < 4 {
			t.Fatalf("condition recovery transitions absent: %+v", report.Transitions)
		}
	}
}
