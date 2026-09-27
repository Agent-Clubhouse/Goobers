package instance

import (
	"testing"
	"time"
)

func TestDiagnosticOTLPIndependentOptIn(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		journal, diagnostics bool
	}{
		{"both off", false, false}, {"journal only", true, false}, {"diagnostics only", false, true}, {"both on", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			if tc.journal {
				cfg.Telemetry.OTLP = &OTLPConfig{Endpoint: "journal.example:4317"}
			}
			if tc.diagnostics {
				cfg.Telemetry.Diagnostics = &DiagnosticsConfig{OTLP: &OTLPConfig{Endpoint: "diagnostics.example:4317"}}
			}
			lookup := func(string) (string, bool) { return "", false }
			journal, err := cfg.ResolveOTLPConfig(lookup)
			if err != nil {
				t.Fatal(err)
			}
			diagnostic := cfg.DiagnosticOTLP()
			if journal.Enabled() != tc.journal || diagnostic.Enabled() != tc.diagnostics {
				t.Fatalf("routing: journal=%v diagnostics=%v", journal.Enabled(), diagnostic.Enabled())
			}
		})
	}
}
func TestOTLPExplicitDisablePrecedesEnvironment(t *testing.T) {
	disabled := false
	cfg := &Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{Endpoint: "configured.example:4317", ExportEnabled: &disabled}}}
	calls := 0
	got, err := cfg.ResolveOTLPConfig(func(string) (string, bool) { calls++; return "invalid ambient setting", true })
	if err != nil || got.Enabled() || calls != 0 {
		t.Fatalf("disable did not override ambient settings: %+v %v calls=%d", got, err, calls)
	}
}
func TestDiagnosticOTLPDoesNotInheritJournalOrEnvironment(t *testing.T) {
	t.Setenv("GOOBERS_OTLP_ENDPOINT", "ambient.example:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://generic.example:4317")
	cfg := &Config{Telemetry: TelemetryConfig{OTLP: &OTLPConfig{Endpoint: "journal.example:4317"}}}
	if cfg.DiagnosticOTLP().Enabled() {
		t.Fatal("diagnostics implicitly enabled")
	}
	disabled := false
	cfg.Telemetry.Diagnostics = &DiagnosticsConfig{OTLP: &OTLPConfig{Endpoint: "diagnostics.example:4317", ExportEnabled: &disabled}}
	if cfg.DiagnosticOTLP().Enabled() {
		t.Fatal("explicit diagnostic opt-out ignored")
	}
}

func TestLoadConfigDiagnosticsWithoutRunTelemetry(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
telemetry:
  enabled: false
  otlp:
    enabled: false
  diagnostics:
    otlp:
      endpoint: https://diagnostics.example.com:4317
      headers:
        authorization:
          env: COMPANY_DIAGNOSTICS_AUTH
      tls:
        caFile: /company/ca.pem
        serverName: diagnostics.example.com
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelemetryEnabled() || !cfg.DiagnosticOTLP().Enabled() {
		t.Fatal("diagnostics incorrectly coupled to run telemetry")
	}
	if cfg.DiagnosticOTLP().Headers["authorization"].Env != "COMPANY_DIAGNOSTICS_AUTH" || cfg.DiagnosticOTLP().TLS.ServerName != "diagnostics.example.com" {
		t.Fatal("diagnostic credentials/TLS not decoded")
	}
}

func TestDiagnosticFleetConfigBounds(t *testing.T) {
	for _, cfg := range []DiagnosticsConfig{
		{Organization: " company"}, {OwnerRef: "owner\nother"}, {Environment: string(make([]byte, 257))},
		{HeartbeatInterval: "0s"}, {HeartbeatInterval: "2h"}, {ProgressTimeout: "30s"}, {ProgressTimeout: "169h"},
		{GaggleOwners: map[string]string{"bad/name": "owner"}}, {GaggleOwners: map[string]string{"gaggle": ""}},
	} {
		if err := cfg.validate(nil); err == nil {
			t.Fatalf("invalid fleet config accepted: %+v", cfg)
		}
	}
	cfg := DiagnosticsConfig{Organization: "company-a", Environment: "production", OwnerRef: "team:operations", GaggleOwners: map[string]string{"gaggle-a": "team:alpha"}, HeartbeatInterval: "10s", ProgressTimeout: "2h"}
	if err := cfg.validate(nil); err != nil {
		t.Fatal(err)
	}
	if cfg.HeartbeatPeriod() != 10*time.Second || cfg.ProgressPeriod() != 2*time.Hour {
		t.Fatal("configured timing not used")
	}
	var defaults *DiagnosticsConfig
	if defaults.HeartbeatPeriod() != 30*time.Second || defaults.ProgressPeriod() != 30*time.Minute {
		t.Fatal("unsafe defaults")
	}
}
