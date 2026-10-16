package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestSchedulerTelemetryReplayStartOptionsPreserveLifecycle(t *testing.T) {
	recovery := localscheduler.NewRecoveryGate()
	options, release := daemonStartupSetupOptions(notifyFlag{}, io.Discard, io.Discard, recovery, nil)
	var configured schedulerSetupOptions
	for _, option := range options {
		option(&configured)
	}
	if !configured.desktopNotifications || configured.startupProgress == nil || configured.claimRecoveryGate != recovery || configured.telemetryReplayStart == nil {
		t.Fatal("startup helper lost existing scheduler setup options")
	}
	select {
	case <-configured.telemetryReplayStart:
		t.Fatal("startup options released replay before readiness")
	default:
	}
	release()
	release() // A repeated readiness signal cannot panic or reconfigure clients.
	select {
	case <-configured.telemetryReplayStart:
	default:
		t.Fatal("readiness did not release replay")
	}
}

func TestSchedulerTelemetryReplayStartCoversAllStreams(t *testing.T) {
	received := make(chan string, 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("decode upload: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer func() {
				if err := gz.Close(); err != nil {
					t.Errorf("close upload decoder: %v", err)
				}
			}()
			reader = gz
		}
		body, err := io.ReadAll(io.LimitReader(reader, 2<<20))
		if err != nil {
			t.Errorf("read upload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case received <- string(body):
		default:
			t.Error("unexpected upload count overflow")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	l := startupGateInstance(t, server.URL)
	root := l.Root
	replayRoot := filepath.Join(root, "telemetry-export", "azure-monitor")
	start := make(chan struct{})
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(t.Context(), l, &wg, withTelemetryReplayStart(start))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setup.Shutdown(context.Background()) })
	if setup.TelemetryReplayStart != start || setup.Telemetry == nil {
		t.Fatal("setup did not preserve its replay signal or local telemetry")
	}
	diagnostics, err := buildDiagnosticExporterWithStores(t.Context(), root, setup, setup.SecretStores)
	if err != nil || diagnostics == nil {
		t.Fatalf("diagnostic setup: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = diagnostics.Shutdown(ctx)
	})
	select {
	case body := <-received:
		t.Fatalf("daemon exporter sent before startup release: %s", body)
	case <-time.After(100 * time.Millisecond):
	}
	stats := telemetry.InspectAzureReplayRoot(replayRoot)
	if !stats.AccountingReady || stats.PendingRecords < 3 {
		t.Fatalf("startup gate prevented local accounting: %+v", stats)
	}
	close(start)
	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 3 {
		select {
		case body := <-received:
			for _, stream := range []string{"traces", "journal", "diagnostics"} {
				if strings.Contains(body, fmt.Sprintf("\"startupTestStream\":%q", stream)) {
					seen[stream] = true
				}
			}
		case <-deadline:
			t.Fatalf("startup release did not resume all streams: %v", seen)
		}
	}
}

func TestSchedulerTelemetryReplayStartFailureClosesUnreleasedExporters(t *testing.T) {
	finalUpload := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case finalUpload <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	l := startupGateInstance(t, server.URL)
	// Failure immediately after telemetry construction: a directory cannot be
	// opened as the rollup database. Preserve any fixture-created file first.
	if _, err := os.Stat(l.TelemetryDB()); err == nil {
		if err := os.Rename(l.TelemetryDB(), l.TelemetryDB()+".test-backup"); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(l.TelemetryDB(), 0o700); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	setup, err := buildSchedulerSetup(ctx, l, &wg, withTelemetryReplayStart(start))
	if setup != nil || err == nil {
		if setup != nil {
			_ = setup.Shutdown(ctx)
		}
		t.Fatalf("obstructed rollup did not fail setup: setup=%v error=%v", setup != nil, err)
	}
	if ctx.Err() != nil {
		t.Fatal("failed setup waited for an unreleased startup signal")
	}
	select {
	case <-finalUpload:
	default:
		t.Fatal("failed setup skipped its bounded final telemetry drain")
	}
	select {
	case <-start:
		t.Fatal("failed setup released the caller-owned readiness signal")
	default:
	}
	if journal.HasCommittedEventSink(l.Root) {
		t.Fatal("failed setup retained its committed-journal subscription")
	}
}

func startupGateInstance(t *testing.T, endpoint string) instance.Layout {
	t.Helper()
	l := instance.NewLayout(initDeterministicDemo(t))
	t.Setenv("STARTUP_GATE_TEST_AZURE", "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint="+endpoint)
	data, err := os.ReadFile(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(string(data), "telemetry: {}\n", "telemetry:\n  enabled: true\n  azureMonitor:\n    connectionString:\n      env: STARTUP_GATE_TEST_AZURE\n", 1)
	if config == string(data) {
		t.Fatal("fixture no longer has default telemetry block")
	}
	if err := os.WriteFile(l.ConfigFile(), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []string{"traces", "journal", "diagnostics"} {
		dir := filepath.Join(l.Root, "telemetry-export", "azure-monitor", stream)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// Synthetic replay-format records test routing, not Azure's schema.
		header, err := json.Marshal(map[string]any{"schema": "goobers.dev/telemetry/azure-replay/v1", "createdAt": time.Now().UTC(), "records": 1})
		if err != nil {
			t.Fatal(err)
		}
		payload := fmt.Sprintf("%s\n{\"startupTestStream\":%q}\n", header, stream)
		if err := os.WriteFile(filepath.Join(dir, "00000000000000000001-startup-test.ndjson"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return l
}
