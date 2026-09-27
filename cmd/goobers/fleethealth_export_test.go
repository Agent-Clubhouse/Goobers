package main

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/goobers/goobers/internal/diagnostics/history"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestFleetHealthProductionExportAndLocalEvidence(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	collector := &serviceHealthCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 4), headers: make(chan metadata.MD, 4)}
	server := grpc.NewServer()
	collectorlogpb.RegisterLogsServiceServer(server, collector)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Diagnostics: &instance.DiagnosticsConfig{
		Organization: "company-alpha", OwnerRef: "team:operations", GaggleOwners: map[string]string{"alpha": "team:alpha"},
		OTLP: &instance.OTLPConfig{Endpoint: listener.Addr().String(), Insecure: true},
	}}}
	now := time.Now().UTC()
	observer := &fleetHealthObserver{reader: &fleetTestReader{now: now}, config: cfg.Telemetry.Diagnostics, instanceID: "instance-alpha", bootID: "boot-alpha", startedAt: now, eligibleSince: map[string]time.Time{}}
	log := openTestInstanceLog(t)
	setup := &schedulerSetup{Config: cfg, SharedRegistry: journal.NewRegistryScrubber(), InstanceLog: log}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startServiceHealth(ctx, t.TempDir(), &daemonIdentity{StartedAt: now}, setup, nil, observer.sample)
	foundOwner := false
	for received := 0; received < 3; {
		select {
		case req := <-collector.requests:
			for _, resource := range req.ResourceLogs {
				for _, scope := range resource.ScopeLogs {
					received += len(scope.LogRecords)
				}
			}
			if strings.Contains(req.String(), "team:alpha") {
				foundOwner = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing local/service/fleet export")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fleet shutdown blocked")
	}
	if !foundOwner {
		t.Fatal("explicit gaggle owner absent from real collector")
	}
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Runner["kind"] == "goobers.fleet.heartbeat" {
			count++
		}
	}
	if count != 0 {
		t.Fatalf("fleet records polluted scheduler journal: %d", count)
	}
	snapshot, err := history.Read(filepath.Join(log.Dir(), "diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 2 {
		t.Fatalf("bounded local records=%d want deployment plus gaggle", len(snapshot.Records))
	}
}
