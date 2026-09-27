package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAzureMonitorConnectionStringReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.yaml")
	raw := []byte(`telemetry:
  enabled: true
  collectionProfile: diagnostic
  azureMonitor:
    connectionString:
      env: APPLICATIONINSIGHTS_CONNECTION_STRING
`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.AzureMonitor == nil || cfg.Telemetry.AzureMonitor.ConnectionString.Env != "APPLICATIONINSIGHTS_CONNECTION_STRING" {
		t.Fatalf("azure monitor config = %+v", cfg.Telemetry.AzureMonitor)
	}
	if cfg.Telemetry.EffectiveCollectionProfile() != TelemetryProfileDiagnostic {
		t.Fatalf("collection profile = %q", cfg.Telemetry.EffectiveCollectionProfile())
	}
}

func TestAzureMonitorConnectionStringMustBeIndirectAndEnabled(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "missing reference",
			raw:  "telemetry:\n  azureMonitor: {}\n",
			want: "connectionString must reference exactly one",
		},
		{
			name: "inline value rejected by strict decode",
			raw:  "telemetry:\n  azureMonitor:\n    connectionString:\n      value: secret\n",
			want: "unknown field",
		},
		{
			name: "disabled telemetry",
			raw:  "telemetry:\n  enabled: false\n  azureMonitor:\n    connectionString:\n      env: APPLICATIONINSIGHTS_CONNECTION_STRING\n",
			want: "cannot be set when telemetry.enabled is false",
		},
		{
			name: "unknown collection profile",
			raw:  "telemetry:\n  collectionProfile: everything\n",
			want: "collectionProfile",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "instance.yaml")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadConfig error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
