package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"

	"github.com/goobers/goobers/internal/diagnostics/featureusage"
	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

type configuredFleetReader struct{ *fleetTestReader }

func (r configuredFleetReader) DiagnosticFeatureConfiguration() map[string]map[string]bool {
	features := map[string]bool{}
	for _, id := range featureusage.IDs() {
		features[id] = false
	}
	features["adapter.codex"] = true
	return map[string]map[string]bool{"alpha": features}
}
func TestFleetUsageProductionStartupReachesStrictBackend(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.WriteFile(layout.ConfigFile(), []byte("existing config"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := instance.EnsureRootIdentity(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.ForGaggle("alpha").RunsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := fleetdiagnostics.New(time.Now, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.SetTenant("tenant", fleetdiagnostics.Tenant{Organization: "synthetic-company"}); err != nil {
		t.Fatal(err)
	}
	for _, gaggle := range []string{"", "alpha"} {
		if err := backend.Enroll("tenant", fleetdiagnostics.Enrollment{Identity: fleetdiagnostics.Identity{Organization: "synthetic-company", DeploymentID: id, InstanceID: id, GaggleID: gaggle, Component: "daemon"}, HeartbeatInterval: 30 * time.Second, MissedIntervals: 2}); err != nil {
			t.Fatal(err)
		}
	}
	receiver, err := fleetdiagnostics.NewReceiver(backend, func(context.Context) (string, error) { return "tenant", nil })
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	collectorlogpb.RegisterLogsServiceServer(server, receiver)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Diagnostics: &instance.DiagnosticsConfig{Organization: "synthetic-company", OTLP: &instance.OTLPConfig{Endpoint: listener.Addr().String(), Insecure: true}}}}
	registry := newInterventionDefinitionRegistry(interventionDefinitionSet{featureDrivers: featureDriverConfiguration([]localscheduler.WorkflowEntry{{Gaggle: "alpha", Workflow: "work", Starter: &runnerFallbackStarter{next: &trackedStarter{}}}})})
	log := openTestInstanceLog(t)
	setup := &schedulerSetup{Config: cfg, SharedRegistry: journal.NewRegistryScrubber(), InstanceLog: log, Interventions: registry}
	now := time.Now().UTC()
	stop := startDaemonHealth(context.Background(), root, &daemonIdentity{StartedAt: now}, setup, nil, configuredFleetReader{&fleetTestReader{now: now}}, func() bool { return true })
	defer stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("production feature records never reached strict backend")
		case <-tick.C:
			reports, err := backend.Reports("tenant")
			if err != nil {
				t.Fatal(err)
			}
			for _, report := range reports {
				if report.GaggleID != "alpha" {
					continue
				}
				for _, feature := range report.Features {
					if feature.FeatureID == "runner.local" && feature.Configured != nil && *feature.Configured && feature.Count != nil && *feature.Count == 0 {
						return
					}
				}
			}
		}
	}
}
func TestFeatureDriversFollowAdmittedSelectionAndReload(t *testing.T) {
	local := localscheduler.WorkflowEntry{Gaggle: "alpha", Workflow: "work", Starter: &runnerFallbackStarter{next: &trackedStarter{}}}
	registry := newInterventionDefinitionRegistry(interventionDefinitionSet{featureDrivers: featureDriverConfiguration([]localscheduler.WorkflowEntry{local})})
	callback := currentFeatureConfiguration(&schedulerSetup{Interventions: registry}, configuredFleetReader{&fleetTestReader{}})
	if got := callback()["alpha"]; !got["runner.local"] || got["runner.engine"] {
		t.Fatal(got)
	}
	local.Starter = &engineStarter{}
	registry.Replace(interventionDefinitionSet{featureDrivers: featureDriverConfiguration([]localscheduler.WorkflowEntry{local})})
	if got := callback()["alpha"]; got["runner.local"] || !got["runner.engine"] {
		t.Fatal(got)
	}
	registry.Replace(interventionDefinitionSet{featureDrivers: map[localscheduler.WorkflowIdentity]string{{Gaggle: "alpha", Workflow: "unknown"}: ""}})
	if got := callback()["alpha"]; got["runner.local"] || got["runner.engine"] {
		t.Fatal(got)
	}
	if _, known := callback()["alpha"]["runner.local"]; known {
		t.Fatal("unknown starter claimed configured false")
	}
}
func BenchmarkFleetLocalJournalBatch(b *testing.B) {
	for _, count := range []int{2, 197} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			log, _, err := journal.OpenInstanceLog(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = log.Close() }()
			attrs := usageHeartbeat("alpha", time.Now().UTC()).Attributes
			attrs["featureId"] = "adapter.codex"
			attrs["configured"] = true
			attrs["count"] = int64(1)
			attrs["windowEnd"] = time.Now().Format(time.RFC3339Nano)
			event := journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": fleetdiagnostics.FeatureEvent, "diagnostic": attrs}}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				for range count {
					if err := log.Append(event); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			path, err := journal.InstanceEventsPath(log.Dir())
			if err != nil {
				b.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(info.Size())/float64(b.N), "journal-B/batch")
		})
	}
}
