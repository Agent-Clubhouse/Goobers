package instance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamedTelemetryConfigRoundTripAndStableSpools(t *testing.T) {
	namedClearOTLPEnvironment(t)
	path := filepath.Join(t.TempDir(), "instance.yaml")
	raw := `telemetry:
  collectionProfile: standard
  exporters:
    - name: collector
      kind: otlp-grpc
      endpoint: https://collector.example.test:4317
      headers:
        authorization: {env: COLLECTOR_AUTH}
      tls: {caFile: /not-mounted-on-worker/ca.pem}
    - name: tenant
      kind: azuremonitor
      connection: {file: /not-mounted-on-worker/connection.txt}
      replay: {maxAge: 48h, maxBytes: 1048576}
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Telemetry.Exporters) != 2 || cfg.Telemetry.OTLP != nil || cfg.Telemetry.AzureMonitor != nil {
		t.Fatalf("unexpected configuration: %+v", cfg.Telemetry)
	}
	root := t.TempDir()
	before, err := cfg.Telemetry.Exporters[1].ReplayRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Telemetry.Exporters[0], cfg.Telemetry.Exporters[1] = cfg.Telemetry.Exporters[1], cfg.Telemetry.Exporters[0]
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := restored.Telemetry.Exporters[0].ReplayRoot(root)
	if before != after || !filepath.IsAbs(after) || !strings.Contains(after, "exporter-tenant") {
		t.Fatalf("spool identity changed: %q -> %q", before, after)
	}
	encoded, err := json.Marshal(restored.Telemetry.Exporters[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"connection"`) {
		t.Fatalf("OTLP round trip wrote an unsupported empty connection: %s", encoded)
	}
	guarded := GuardedCredentialPaths(restored)
	if len(guarded) != 1 || guarded[0] != "/not-mounted-on-worker/connection.txt" {
		t.Fatalf("named secret not guarded: %q", guarded)
	}
}

func TestNamedTelemetryConfigRejectsInvalidWholeList(t *testing.T) {
	namedClearOTLPEnvironment(t)
	for _, tc := range []struct{ name, body, want string }{
		{"unsupported", "- {name: bad, kind: otlp-http, endpoint: 'https://collector.test'}", "6502"},
		{"unknown kind", "- {name: bad, kind: unknown}", "otlp-grpc or azuremonitor"},
		{"duplicate", "- {name: same, kind: otlp-grpc, endpoint: 'https://a.test'}\n    - {name: same, kind: otlp-grpc, endpoint: 'https://b.test'}", "unique"},
		{"traversal", "- {name: '../other', kind: azuremonitor, connection: {env: SECRET}}", "names"},
		{"case alias", "- {name: Tenant, kind: azuremonitor, connection: {env: SECRET}}", "names"},
		{"missing endpoint", "- {name: bad, kind: otlp-grpc}", "endpoint is required"},
		{"missing connection", "- {name: bad, kind: azuremonitor}", "exactly one"},
		{"inline secret", "- {name: bad, kind: azuremonitor, connection: {value: private}}", "unknown field"},
		{"unknown store", "- {name: bad, kind: azuremonitor, connection: {store: missing/secret}}", "missing"},
		{"plaintext remote", "- {name: bad, kind: otlp-grpc, endpoint: 'http://collector.example:4317', insecure: true}", "loopback"},
		{"tls conflict", "- {name: bad, kind: otlp-grpc, endpoint: '127.0.0.1:4317', insecure: true, tls: {caFile: ca.pem}}", "conflicts"},
		{"mixed kind fields", "- {name: bad, kind: azuremonitor, endpoint: 'https://collector.test', connection: {env: SECRET}}", "OTLP settings"},
		{"unsupported replay", "- {name: bad, kind: otlp-grpc, endpoint: 'https://collector.test', replay: {maxAge: 24h}}", "does not support disk replay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "instance.yaml")
			if err := os.WriteFile(path, []byte("telemetry:\n  exporters:\n    "+tc.body+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if cfg != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("cfg=%v err=%v, want %q", cfg, err, tc.want)
			}
		})
	}
}

func TestNamedTelemetryRejectsLegacyAndEnvironmentMixing(t *testing.T) {
	namedClearOTLPEnvironment(t)
	good := TelemetryExporterConfig{Name: "collector", Kind: "otlp-grpc", Endpoint: "https://collector.test"}
	for _, legacy := range []TelemetryConfig{{OTLP: &OTLPConfig{Endpoint: "https://legacy.test"}}, {AzureMonitor: &AzureMonitorConfig{ConnectionString: TokenRef{Env: "SECRET"}}}} {
		legacy.Exporters = []TelemetryExporterConfig{good}
		if err := legacy.validate(nil, true); err == nil {
			t.Fatal("legacy/list duplicate route accepted")
		}
	}
	cfg := &Config{Telemetry: TelemetryConfig{Exporters: []TelemetryExporterConfig{good}}}
	path := filepath.Join(t.TempDir(), "instance.yaml")
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(OTLPEndpointEnv, "https://ambient.test")
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("environment silently altered named routing: %v", err)
	}
}

func namedClearOTLPEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{OTLPEndpointEnv, OTLPInsecureEnv} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}
