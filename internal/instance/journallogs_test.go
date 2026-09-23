package instance

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestOTLPJournalLogsEnabled(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		otlp OTLPConfig
		want bool
	}{
		{"no endpoint", OTLPConfig{}, false},
		{"default on with endpoint", OTLPConfig{Endpoint: "localhost:4317"}, true},
		{"explicit on", OTLPConfig{Endpoint: "localhost:4317", JournalLogs: &on}, true},
		{"explicit off", OTLPConfig{Endpoint: "localhost:4317", JournalLogs: &off}, false},
		{"export disabled", OTLPConfig{Endpoint: "localhost:4317", ExportEnabled: &off}, false},
		{"logs cannot enable export", OTLPConfig{JournalLogs: &on}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Telemetry: TelemetryConfig{OTLP: &tc.otlp}}
			got, err := cfg.ResolveOTLPConfig(func(string) (string, bool) { return "", false })
			if err != nil {
				t.Fatal(err)
			}
			if got.JournalLogsEnabled() != tc.want {
				t.Fatalf("JournalLogsEnabled() = %v, want %v", got.JournalLogsEnabled(), tc.want)
			}
		})
	}
}

func TestOTLPJournalLogsWireFormat(t *testing.T) {
	for _, document := range []string{
		`{"telemetry":{"otlp":{"endpoint":"localhost:4317","journalLogs":false}}}`,
		"telemetry:\n  otlp:\n    endpoint: localhost:4317\n    journalLogs: false\n",
	} {
		var cfg Config
		if err := yaml.UnmarshalStrict([]byte(document), &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Telemetry.OTLP.JournalLogs == nil || cfg.Telemetry.OTLP.JournalLogsEnabled() {
			t.Fatal("explicit false lost during decode")
		}
		data, err := json.Marshal(cfg.Telemetry.OTLP)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["journalLogs"] != false {
			t.Fatalf("journalLogs not retained: %s", data)
		}
	}
}
