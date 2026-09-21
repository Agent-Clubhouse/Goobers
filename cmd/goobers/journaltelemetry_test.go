package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/encoding/prototext"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/telemetry"
	"sigs.k8s.io/yaml"
)

func TestConfigureOTLPJournalLogsGate(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		otlp instance.OTLPConfig
		want bool
	}{
		{"default on", instance.OTLPConfig{Endpoint: "localhost:4317"}, true},
		{"explicit false", instance.OTLPConfig{Endpoint: "localhost:4317", JournalLogs: &off}, false},
		{"export disabled", instance.OTLPConfig{Endpoint: "localhost:4317", ExportEnabled: &off}, false},
		{"no endpoint", instance.OTLPConfig{JournalLogs: &on}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := telemetry.Config{}
			if err := configureOTLP(context.Background(), &cfg, tc.otlp, journal.NewRegistryScrubber(), nil); err != nil {
				t.Fatal(err)
			}
			if cfg.JournalLogs != tc.want {
				t.Fatalf("JournalLogs = %v, want %v", cfg.JournalLogs, tc.want)
			}
			if tc.otlp.Enabled() && cfg.Exporter != telemetry.ExporterOTLP {
				t.Fatal("journal log gate changed native trace and metric export")
			}
		})
	}
}

func TestJournalRedactExportsLiveOTLPLog(t *testing.T) {
	for _, mode := range []string{"default on", "explicit false", "export disabled", "no endpoint", "collector rejects", "invalid TLS"} {
		t.Run(mode, func(t *testing.T) {
			for _, key := range []string{instance.OTLPEndpointEnv, instance.OTLPInsecureEnv} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			collector := &routingCollector{failDiagnostics: mode == "collector rejects"}
			endpoint := startRoutingCollector(t, collector)
			root := initDemo(t)
			runID, blobPath := writeRunWithLeakedArtifact(t, root)
			layout := instance.NewLayout(root)
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			off := false
			t.Setenv("JOURNAL_LOGS_TEST_TOKEN", "journal-log-test-credential")
			cfg.Telemetry.OTLP = &instance.OTLPConfig{
				Endpoint: endpoint, Insecure: true,
				Headers: map[string]instance.TokenRef{"authorization": {Env: "JOURNAL_LOGS_TEST_TOKEN"}},
			}
			switch mode {
			case "explicit false":
				cfg.Telemetry.OTLP.JournalLogs = &off
			case "export disabled":
				cfg.Telemetry.OTLP.ExportEnabled = &off
			case "no endpoint":
				cfg.Telemetry.OTLP = nil
			case "invalid TLS":
				cfg.Telemetry.OTLP.Insecure = false
				cfg.Telemetry.OTLP.TLS = &instance.OTLPTLSConfig{CAFile: filepath.Join(root, "missing-ca.pem")}
			}
			data, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(layout.ConfigFile(), data, 0o600); err != nil {
				t.Fatal(err)
			}
			secretFile := filepath.Join(root, "redaction-secret")
			if err := os.WriteFile(secretFile, []byte(cliLeak), 0o600); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runArgs(t, "journal", "redact", "--run", runID, "--path", blobPath,
				"--reason", "remove exposed credential", "--secret-file", secretFile, root)
			if code != 0 {
				t.Fatalf("redact failed: %d %s", code, stderr)
			}
			if journal.HasCommittedEventSink(root) {
				t.Fatal("command leaked its root registration")
			}
			if strings.Contains(stdout, `"SpanContext"`) || strings.Contains(stdout, `"Resource"`) {
				t.Fatalf("telemetry polluted command output: %s", stdout)
			}
			collector.mu.Lock()
			defer collector.mu.Unlock()
			logs := 0
			for _, observation := range collector.observations {
				if observation.signal != "logs" {
					t.Fatalf("direct journal command exported %s; want logs only", observation.signal)
				}
				request := &collectorlog.ExportLogsServiceRequest{}
				if err := prototext.Unmarshal([]byte(observation.payload), request); err != nil {
					t.Fatal(err)
				}
				for _, resource := range request.ResourceLogs {
					for _, scope := range resource.ScopeLogs {
						logs += len(scope.LogRecords)
					}
				}
				if observation.authorization != "journal-log-test-credential" {
					t.Fatalf("native collector credentials not reused: %q", observation.authorization)
				}
				if !strings.Contains(observation.payload, string(journal.EventRedaction)) ||
					!strings.Contains(observation.payload, runID) {
					t.Fatalf("not the live redaction: %s", observation.payload)
				}
				if strings.Contains(observation.payload, cliLeak) {
					t.Fatal("collector received leaked secret")
				}
			}
			wantLog := mode == "default on" || mode == "collector rejects"
			if (logs > 0) != wantLog {
				t.Fatalf("logs = %d, want any = %v; stderr=%s", logs, wantLog, stderr)
			}
			if wantLog && logs != 1 {
				t.Fatalf("logs = %d, want exactly the new redaction, not seeded history", logs)
			}
			if (mode == "collector rejects" || mode == "invalid TLS") && !strings.Contains(stderr, "warning: journal OTLP logs") {
				t.Fatalf("failed export did not warn: %s", stderr)
			}
		})
	}
}

func TestClaimsReleaseExportsLiveOTLPLog(t *testing.T) {
	for _, key := range []string{instance.OTLPEndpointEnv, instance.OTLPInsecureEnv} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	root := initDemo(t)
	runID := strings.Repeat("a", 32)
	seedJournaledClaim(t, root, "issue-9", runID, false)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	collector := &routingCollector{}
	cfg.Telemetry.OTLP = &instance.OTLPConfig{Endpoint: startRoutingCollector(t, collector), Insecure: true}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.ConfigFile(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "claims", "release", "--force", "issue-9", root)
	if code != 0 {
		t.Fatalf("release failed: %d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if journal.HasCommittedEventSink(root) {
		t.Fatal("command leaked its root registration")
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	records := 0
	for _, observation := range collector.observations {
		if observation.signal != "logs" {
			t.Fatalf("direct command exported %s, want logs only", observation.signal)
		}
		request := &collectorlog.ExportLogsServiceRequest{}
		if err := prototext.Unmarshal([]byte(observation.payload), request); err != nil {
			t.Fatal(err)
		}
		for _, resource := range request.ResourceLogs {
			for _, scope := range resource.ScopeLogs {
				for _, record := range scope.LogRecords {
					records++
					body := record.Body.GetStringValue()
					if !strings.Contains(body, string(journal.EventClaimForceReleased)) ||
						!strings.Contains(body, "issue-9") || !strings.Contains(body, runID) {
						t.Fatalf("unexpected scheduler event: %s", body)
					}
				}
			}
		}
	}
	if records != 1 {
		t.Fatalf("records = %d, want exactly the new claim release; stderr=%s", records, stderr)
	}
}

func TestClaimLockDiagnosticsExportLiveOTLPLogs(t *testing.T) {
	for _, mode := range []string{"slow", "timeout", "ordinary", "existing owner"} {
		t.Run(mode, func(t *testing.T) {
			for _, key := range []string{instance.OTLPEndpointEnv, instance.OTLPInsecureEnv} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			root := initDemo(t)
			layout := instance.NewLayout(root)
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}

			collector := &routingCollector{}
			cfg.Telemetry.OTLP = &instance.OTLPConfig{Endpoint: startRoutingCollector(t, collector), Insecure: true}
			if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(layout.SchedulerDir(), claimLockFileName)
			var existing *telemetry.Client
			if mode == "existing owner" {
				export := telemetry.Config{JournalLogsOnly: true, JournalRoot: root}
				if err := configureOTLP(context.Background(), &export, *cfg.Telemetry.OTLP, journal.NewRegistryScrubber(), nil); err != nil {
					t.Fatal(err)
				}
				existing, err = telemetry.New(context.Background(), export)
				if err != nil {
					t.Fatal(err)
				}
				defer existing.Shutdown(context.Background())
			}
			threshold := time.Hour
			wantType := journal.EventType("")
			if mode == "slow" || mode == "existing owner" {
				threshold = -time.Nanosecond
				wantType = journal.EventClaimLockSlow
			}
			if mode == "timeout" {
				holder, err := lock.TryAcquire(lockPath)
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Release()
				wantType = journal.EventClaimLockTimeout
			}
			called := false
			err = withClaimLockBounds(lockPath, "test.telemetry", 20*time.Millisecond, threshold, claimLockEventContext{}, func() error {
				called = true
				if journal.HasCommittedEventSink(root) != (existing != nil) {
					t.Fatal("ordinary lock callback changed telemetry ownership")
				}
				return nil
			})
			if mode == "timeout" {
				var timeoutErr *claimsLockTimeoutError
				if !errors.As(err, &timeoutErr) || called {
					t.Fatalf("timeout semantics changed: error=%v callback=%v", err, called)
				}
				if competing, err := lock.TryAcquire(lockPath); !errors.Is(err, lock.ErrHeld) {
					if err == nil {
						_ = competing.Release()
					}
					t.Fatalf("timeout released another holder: %v", err)
				}
			} else if err != nil || !called {
				t.Fatalf("lock operation failed: error=%v callback=%v", err, called)
			}
			if existing != nil {
				if !journal.HasCommittedEventSink(root) {
					t.Fatal("diagnostic closed the existing owner's registration")
				}
				drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := existing.Shutdown(drain)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
			}
			if journal.HasCommittedEventSink(root) {
				t.Fatal("diagnostic leaked its telemetry owner")
			}
			collector.mu.Lock()
			defer collector.mu.Unlock()
			records := 0
			for _, observation := range collector.observations {
				if observation.signal != "logs" {
					t.Fatalf("diagnostic exported %s, want logs only", observation.signal)
				}
				request := &collectorlog.ExportLogsServiceRequest{}
				if err := prototext.Unmarshal([]byte(observation.payload), request); err != nil {
					t.Fatal(err)
				}
				for _, resource := range request.ResourceLogs {
					for _, scope := range resource.ScopeLogs {
						for _, record := range scope.LogRecords {
							records++
							if !strings.Contains(record.Body.GetStringValue(), string(wantType)) {
								t.Fatalf("wrong diagnostic: %s", record.Body.GetStringValue())
							}
						}
					}
				}
			}
			want := 1
			if mode == "ordinary" {
				want = 0
			}
			if records != want {
				t.Fatalf("records=%d, want %d", records, want)
			}
		})
	}
}

func TestCommandJournalTelemetrySharesRootLifetime(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	collector := &routingCollector{}
	cfg.Telemetry.OTLP = &instance.OTLPConfig{Endpoint: startRoutingCollector(t, collector), Insecure: true}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.ConfigFile(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	first := startCommandJournalTelemetry(layout, &stderr)
	second := startCommandJournalTelemetry(layout, &stderr)
	defer first()
	defer second()
	first()
	if !journal.HasCommittedEventSink(root) {
		t.Fatalf("first close removed overlapping owner's registration: %s", stderr.String())
	}
	second()
	if journal.HasCommittedEventSink(root) {
		t.Fatal("last close did not remove registration")
	}
}
