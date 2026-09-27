package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestTelemetryConfigureWritesOnlyConnectionStringReference(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := instance.WriteConfig(layout.ConfigFile(), &instance.Config{}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runTelemetryConfigure([]string{"--connection-string-env", "TENANT_APPINSIGHTS", root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("configure exit=%d stderr=%q", code, stderr.String())
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.AzureMonitor == nil || cfg.Telemetry.AzureMonitor.ConnectionString.Env != "TENANT_APPINSIGHTS" || cfg.Telemetry.EffectiveCollectionProfile() != instance.TelemetryProfileStandard {
		t.Fatalf("configured telemetry = %+v", cfg.Telemetry)
	}
	data, err := os.ReadFile(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "InstrumentationKey=") {
		t.Fatalf("instance config contains a connection-string value: %s", data)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runTelemetryConfigure([]string{"--disable", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("disable exit=%d stderr=%q", code, stderr.String())
	}
	cfg, err = instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.AzureMonitor != nil {
		t.Fatalf("Azure destination remained after disable: %+v", cfg.Telemetry.AzureMonitor)
	}
}

func TestTelemetryConfigureRejectsPastedSecretWithoutWriting(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := instance.WriteConfig(layout.ConfigFile(), &instance.Config{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(layout.ConfigFile())
	secret := "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=https://example.invalid"
	var stdout, stderr bytes.Buffer
	if code := runTelemetryConfigure([]string{"--connection-string-env", secret, root}, &stdout, &stderr); code != 2 {
		t.Fatalf("configure exit=%d stderr=%q", code, stderr.String())
	}
	after, _ := os.ReadFile(layout.ConfigFile())
	if !bytes.Equal(before, after) {
		t.Fatal("pasted secret changed instance config")
	}
	if strings.Contains(stderr.String(), "00000000") || strings.Contains(stdout.String(), "00000000") {
		t.Fatalf("pasted secret reflected in output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestTelemetryTestResolvesReferenceWithoutPrintingSecret(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	enabled := true
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{
		Enabled: &enabled, CollectionProfile: instance.TelemetryProfileStandard,
		AzureMonitor: &instance.AzureMonitorConfig{ConnectionString: instance.TokenRef{Env: "TENANT_APPINSIGHTS_TEST"}},
	}}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	secret := "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=https://example.invalid"
	t.Setenv("TENANT_APPINSIGHTS_TEST", secret)
	original := testAzureMonitorConnectivity
	t.Cleanup(func() { testAzureMonitorConnectivity = original })
	testAzureMonitorConnectivity = func(_ context.Context, got string, client *http.Client) (telemetry.AzureMonitorConnectivityResult, error) {
		if got != secret || client != nil {
			t.Fatalf("connectivity input = %q, %v", got, client)
		}
		return telemetry.AzureMonitorConnectivityResult{Schema: "goobers.dev/telemetry/connectivity/v1", Accepted: true, RecordID: "record-1", ObservedAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runTelemetryTest([]string{"--json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("test exit=%d stderr=%q", code, stderr.String())
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, secret) || strings.Contains(combined, "00000000") {
		t.Fatalf("connectivity output leaked secret: %q", combined)
	}
	if !strings.Contains(stdout.String(), `"recordId":"record-1"`) {
		t.Fatalf("connectivity output = %q", stdout.String())
	}
}
