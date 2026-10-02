package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestNamedTelemetryConnectivitySelectsOnlyRequestedDestination(t *testing.T) {
	unsetTestEnv(t, instance.OTLPEndpointEnv)
	unsetTestEnv(t, instance.OTLPInsecureEnv)
	var first, second atomic.Int32
	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { first.Add(1); w.WriteHeader(200) }))
	t.Cleanup(one.Close)
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { second.Add(1); w.WriteHeader(200) }))
	t.Cleanup(two.Close)
	t.Setenv("NAMED_PROBE_ONE", "InstrumentationKey=private-key-one;IngestionEndpoint="+one.URL)
	t.Setenv("NAMED_PROBE_TWO", "InstrumentationKey=private-key-two;IngestionEndpoint="+two.URL)
	layout := instance.NewLayout(t.TempDir())
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Exporters: []instance.TelemetryExporterConfig{{Name: "one", Kind: "azuremonitor", Connection: instance.TokenRef{Env: "NAMED_PROBE_ONE"}}, {Name: "two", Kind: "azuremonitor", Connection: instance.TokenRef{Env: "NAMED_PROBE_TWO"}}}}}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runTelemetryTest([]string{"--destination", "two", "--json", layout.Root}, &stdout, &stderr); code != 0 {
		t.Fatalf("probe failed %d: %s", code, stderr.String())
	}
	if first.Load() != 0 || second.Load() != 1 || strings.Contains(stdout.String()+stderr.String(), "private-key-") {
		t.Fatalf("probe routing/leak: counts %d %d output %s %s", first.Load(), second.Load(), &stdout, &stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runTelemetryTest([]string{layout.Root}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--destination") {
		t.Fatalf("implicit probe selection: %d %s", code, &stderr)
	}
	if code := runTelemetryConfigure([]string{"--disable", layout.Root}, &stdout, &stderr); code != 2 {
		t.Fatalf("legacy configure silently changed named list: %d", code)
	}
	after, err := os.ReadFile(layout.ConfigFile())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("probe/configure changed named configuration")
	}
}
