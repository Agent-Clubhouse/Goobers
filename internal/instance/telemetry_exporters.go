package instance

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// TelemetryExporterConfig identifies an operator-owned destination. Connection
// is an Application Insights connection-string reference; OTLP authentication
// uses Headers. Transport settings have the same semantics as legacy OTLP.
type TelemetryExporterConfig struct {
	Name        string                    `json:"name" yaml:"name"`
	Kind        string                    `json:"kind" yaml:"kind"`
	Endpoint    string                    `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Connection  TokenRef                  `json:"connection,omitempty,omitzero" yaml:"connection,omitempty"`
	Headers     map[string]TokenRef       `json:"headers,omitempty" yaml:"headers,omitempty"`
	Insecure    bool                      `json:"insecure,omitempty" yaml:"insecure,omitempty"`
	TLS         *OTLPTLSConfig            `json:"tls,omitempty" yaml:"tls,omitempty"`
	JournalLogs *bool                     `json:"journalLogs,omitempty" yaml:"journalLogs,omitempty"`
	Replay      *AzureMonitorReplayConfig `json:"replay,omitempty" yaml:"replay,omitempty"`
}

var telemetryExporterName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// OTLPConfig returns this destination's transport configuration without applying
// process environment overrides, which belong exclusively to the legacy block.
func (c TelemetryExporterConfig) OTLPConfig() OTLPConfig {
	return OTLPConfig{Endpoint: c.Endpoint, Insecure: c.Insecure, Headers: c.Headers, TLS: c.TLS, JournalLogs: c.JournalLogs}
}

func (c TelemetryConfig) validateExporters(stores map[string]bool, enabled bool) error {
	if len(c.Exporters) == 0 {
		return nil
	}
	if len(c.Exporters) > 16 {
		return fmt.Errorf("telemetry.exporters supports at most 16 destinations")
	}
	if !enabled {
		return fmt.Errorf("telemetry.exporters cannot be set when telemetry.enabled is false")
	}
	if c.OTLP != nil || c.AzureMonitor != nil {
		return fmt.Errorf("telemetry.exporters cannot be combined with telemetry.otlp or telemetry.azureMonitor; migrate the legacy destinations into the list")
	}
	names := make(map[string]bool)
	for _, exporter := range c.Exporters {
		if !telemetryExporterName.MatchString(exporter.Name) || names[exporter.Name] {
			return fmt.Errorf("telemetry.exporters names must be unique and match [a-z][a-z0-9-]{0,63}")
		}
		names[exporter.Name] = true
		if err := exporter.validate(stores); err != nil {
			return fmt.Errorf("telemetry.exporters[%s]: %w", exporter.Name, err)
		}
	}
	return nil
}

func (c TelemetryExporterConfig) validate(stores map[string]bool) error {
	switch c.Kind {
	case "otlp-grpc":
		if c.Connection.Configured() || c.Replay != nil {
			return fmt.Errorf("OTLP uses headers for authentication and does not support disk replay")
		}
		if c.Endpoint == "" {
			return fmt.Errorf("endpoint is required")
		}
		otlp := c.OTLPConfig()
		return (TelemetryConfig{OTLP: &otlp}).validate(stores, true)
	case "azuremonitor":
		if c.Endpoint != "" || c.Insecure || c.TLS != nil || len(c.Headers) != 0 || c.JournalLogs != nil {
			return fmt.Errorf("azuremonitor selects its endpoint from connection and signals from collectionProfile; OTLP settings are not supported")
		}
		return (TelemetryConfig{AzureMonitor: &AzureMonitorConfig{ConnectionString: c.Connection, Replay: c.Replay}}).validate(stores, true)
	default:
		return fmt.Errorf("kind must be otlp-grpc or azuremonitor; additional transports are tracked in https://github.com/Agent-Clubhouse/Goobers/issues/6502")
	}
}

// ReplayRoot is stable across list reorder and process restarts. Prefixing the
// directory also avoids reserved Windows device names.
func (c TelemetryExporterConfig) ReplayRoot(root string) (string, error) {
	return filepath.Abs(filepath.Join(root, "telemetry-export", "destinations", "exporter-"+c.Name))
}

// NamedJournalEnabled reports whether any named destination requests journals.
func (c TelemetryConfig) NamedJournalEnabled() bool {
	for _, d := range c.Exporters {
		if d.Kind == "azuremonitor" && c.EffectiveCollectionProfile().IncludesJournal() {
			return true
		}
		if d.Kind == "otlp-grpc" && d.OTLPConfig().JournalLogsEnabled() {
			return true
		}
	}
	return false
}

// NamedAzureEnabled reports whether named direct export is explicitly configured.
func (c TelemetryConfig) NamedAzureEnabled() bool {
	for _, d := range c.Exporters {
		if d.Kind == "azuremonitor" {
			return true
		}
	}
	return false
}
