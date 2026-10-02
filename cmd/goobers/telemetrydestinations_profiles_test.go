package main

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestNamedTelemetryAzureProfileSignalParity(t *testing.T) {
	for _, profile := range []instance.TelemetryCollectionProfile{instance.TelemetryProfileHealth, instance.TelemetryProfileJournal, instance.TelemetryProfileStandard, instance.TelemetryProfileDiagnostic} {
		t.Run(string(profile), func(t *testing.T) {
			var mu sync.Mutex
			var payloads []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader, err := gzip.NewReader(r.Body)
				if err != nil {
					http.Error(w, "gzip", 400)
					return
				}
				defer func() { _ = reader.Close() }()
				body, _ := io.ReadAll(reader)
				mu.Lock()
				payloads = append(payloads, string(body))
				mu.Unlock()
				w.WriteHeader(200)
			}))
			t.Cleanup(server.Close)
			t.Setenv("NAMED_PROFILE_CONNECTION", "InstrumentationKey=fixture;IngestionEndpoint="+server.URL)
			disabled := false
			cfg := &instance.Config{Telemetry: instance.TelemetryConfig{CollectionProfile: profile, Exporters: []instance.TelemetryExporterConfig{{Name: "tenant", Kind: "azuremonitor", Connection: instance.TokenRef{Env: "NAMED_PROFILE_CONNECTION"}, Replay: &instance.AzureMonitorReplayConfig{Enabled: &disabled}}}}}
			root := t.TempDir()
			registry, scrubber := journal.DefaultScrubber()
			client, err := buildTelemetryClient(t.Context(), instance.NewLayout(root), scrubber, registry, cfg.Telemetry, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			diagnostic, err := buildDiagnosticExporterWithStores(t.Context(), root, &schedulerSetup{Config: cfg, SharedRegistry: registry}, nil)
			if err != nil {
				t.Fatal(err)
			}
			log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"), journal.WithScrubber(scrubber))
			if err != nil {
				t.Fatal(err)
			}
			log.AppendBestEffort(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"fixture": "profile-event"}})
			_, span, err := client.StartSchedulerSpan(t.Context(), telemetry.SchedulerAttributes{Gaggle: "fixture", WorkflowID: "fixture", Action: "dispatch"})
			if err != nil {
				t.Fatal(err)
			}
			span.End()
			diagnostic.Emit(serviceHealthDiagnosticRecord(journal.Event{Time: time.Now(), Runner: map[string]any{"schemaVersion": 1, "machineName": "named-profile-machine", "accountName": "named-profile-account"}}, profile.IncludesHostIdentity()))
			_ = log.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = client.Shutdown(ctx)
			_ = diagnostic.Shutdown(ctx)
			mu.Lock()
			body := strings.Join(payloads, "\n")
			mu.Unlock()
			for _, want := range []struct {
				marker  string
				enabled bool
			}{{"goobers.service.health", true}, {"goobers.journal", profile.IncludesJournal()}, {"scheduler/dispatch", profile.IncludesTraces()}, {"named-profile-machine", profile.IncludesHostIdentity()}, {"named-profile-account", profile.IncludesHostIdentity()}} {
				if strings.Contains(body, want.marker) != want.enabled {
					t.Fatalf("profile %s marker %s expected=%t: %s", profile, want.marker, want.enabled, body)
				}
			}
		})
	}
}

func TestNamedTelemetryAzureReplayHealthSurvivesRestartAndReorder(t *testing.T) {
	var available atomic.Bool
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !available.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(good.Close)
	t.Setenv("NAMED_REPLAY_BAD", "InstrumentationKey=fixture;IngestionEndpoint="+bad.URL)
	t.Setenv("NAMED_REPLAY_GOOD", "InstrumentationKey=fixture;IngestionEndpoint="+good.URL)
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Exporters: []instance.TelemetryExporterConfig{{Name: "bad", Kind: "azuremonitor", Connection: instance.TokenRef{Env: "NAMED_REPLAY_BAD"}}, {Name: "good", Kind: "azuremonitor", Connection: instance.TokenRef{Env: "NAMED_REPLAY_GOOD"}}}}}
	root := t.TempDir()
	layout := instance.NewLayout(root)
	registry, scrubber := journal.DefaultScrubber()
	start := func() (*telemetry.Client, *telemetry.ExporterHealth) {
		t.Helper()
		health := newTelemetryExporterHealth(cfg)
		client, err := buildTelemetryClient(t.Context(), layout, scrubber, registry, cfg.Telemetry, nil, health, nil)
		if err != nil {
			t.Fatal(err)
		}
		return client, health
	}
	client, health := start()
	_, span, err := client.StartSchedulerSpan(t.Context(), telemetry.SchedulerAttributes{Gaggle: "fixture", WorkflowID: "fixture", Action: "dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	span.End()
	flushCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	if err := client.Flush(flushCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	shutdown := func(client *telemetry.Client) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		if err := client.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	waitNamedTelemetry(t, func() bool {
		s := health.Snapshot()
		return s.Destinations["bad"].Replay.ActiveFailure && s.Destinations["good"].Replay.LastSuccess != nil
	})
	before := health.Snapshot().Destinations["bad"].Replay
	if before.PendingRecords != 1 || before.FailureClass == "" {
		t.Fatalf("lost per-destination replay health: %+v", before)
	}
	shutdown(client)
	cfg.Telemetry.Exporters[0], cfg.Telemetry.Exporters[1] = cfg.Telemetry.Exporters[1], cfg.Telemetry.Exporters[0]
	available.Store(true)
	client, health = start()
	waitNamedTelemetry(t, func() bool {
		s := health.Snapshot().Destinations["bad"].Replay
		return !s.ActiveFailure && s.PendingRecords == 0 && s.LastSuccess != nil
	})
	shutdown(client)
}

func waitNamedTelemetry(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("named telemetry condition did not become ready")
}
