package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/telemetry"
)

func writeSyntheticCollectorProfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic-profile.json")
	raw := `{"profile_version":1,"enable_env":"SYNTHETIC_UPLOAD","endpoint":"https://collector.example.invalid/ingest","routing_key":{"env":"SYNTHETIC_ROUTE"},"instrumentation_key":{"env":"SYNTHETIC_KEY"},"mappings":[{"signal":"journal","event_name":"Synthetic.Journal","envelope_version":"4.0","fields":{"body":{"source":"journal.raw_json"}}}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOneCollectorCapabilityAndConfigurePreservesPublicSettings(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"telemetry", "onecollector", "--capabilities", "--json"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"supported":true`) {
		t.Fatal(code, stdout.String(), stderr.String())
	}
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "public-disabled", true: "public-enabled"}[enabled], func(t *testing.T) {
			root := t.TempDir()
			layout := instance.NewLayout(root)
			cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Enabled: &enabled}}
			if enabled {
				cfg.Telemetry.Exporters = []instance.TelemetryExporterConfig{{Name: "synthetic", Kind: "otlp-grpc", Endpoint: "https://public.example.invalid:4317"}}
			}
			if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
				t.Fatal(err)
			}
			profile := writeSyntheticCollectorProfile(t)
			stdout.Reset()
			stderr.Reset()
			args := []string{"--onecollector-profile-file", profile, "--json", root}
			if code := runTelemetryConfigure(args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"changed":true`) {
				t.Fatal(code, stdout.String(), stderr.String())
			}
			got, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			if got.TelemetryEnabled() != enabled || len(got.Telemetry.Exporters) != len(cfg.Telemetry.Exporters) || got.Telemetry.OneCollector.ProfileFile != profile {
				t.Fatal("public settings changed")
			}
			before, err := os.ReadFile(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			stdout.Reset()
			if code := runTelemetryConfigure(args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"changed":false`) {
				t.Fatal(code, stdout.String(), stderr.String())
			}
			after, _ := os.ReadFile(layout.ConfigFile())
			if !bytes.Equal(before, after) {
				t.Fatal("idempotent configure rewrote config")
			}
			stdout.Reset()
			if code := runTelemetryConfigure([]string{"--onecollector-profile-file", writeSyntheticCollectorProfile(t), root}, &stdout, &stderr); code != 1 {
				t.Fatal(code)
			}
		})
	}
}

func TestOneCollectorRuntimeExplicitGateAndSecretRegistration(t *testing.T) {
	profile := writeSyntheticCollectorProfile(t)
	source := &instance.OneCollectorConfig{ProfileFile: profile}
	t.Setenv("SYNTHETIC_ROUTE", "")
	t.Setenv("SYNTHETIC_KEY", "")
	for _, optIn := range []string{"", "true", "0"} {
		t.Setenv("SYNTHETIC_UPLOAD", optIn)
		var cfg telemetry.Config
		if err := runtimewiring.ConfigureOneCollector(t.Context(), &cfg, source, nil, nil); err != nil || cfg.OneCollector != nil {
			t.Fatal(err)
		}

	}
	t.Setenv("SYNTHETIC_UPLOAD", "1")
	var cfg telemetry.Config
	if err := runtimewiring.ConfigureOneCollector(t.Context(), &cfg, nil, nil, nil); err != nil || cfg.OneCollector != nil {
		t.Fatal("env alone configured exporter", err)
	}
	if err := runtimewiring.ConfigureOneCollector(t.Context(), &cfg, source, nil, nil); err == nil {
		t.Fatal("missing keys accepted")
	}
	t.Setenv("SYNTHETIC_ROUTE", "synthetic-routing-secret")
	t.Setenv("SYNTHETIC_KEY", "synthetic-instrumentation-secret")
	registry := journal.NewRegistryScrubber()
	if err := runtimewiring.ConfigureOneCollector(t.Context(), &cfg, source, registry, nil); err != nil || cfg.OneCollector == nil {
		t.Fatal(err)
	}
	if got := string(registry.Scrub([]byte("synthetic-routing-secret synthetic-instrumentation-secret"))); strings.Contains(got, "synthetic-") {
		t.Fatal("secrets not registered")
	}
	disabled := false
	client, err := buildTelemetryClient(t.Context(), instance.NewLayout(t.TempDir()), registry, registry, instance.TelemetryConfig{Enabled: &disabled, OneCollector: source}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("public opt-out blocked independent uploader")
	}
	if err := client.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}
