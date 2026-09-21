package instance

import (
	"fmt"
	"sort"
)

// DiagnosticsConfig controls diagnostic export independently of run telemetry.
// Its collector is explicit: neither the journal collector nor ambient OTLP
// settings can opt an operator into exporting instance identity.
type DiagnosticsConfig struct {
	OTLP *OTLPConfig `json:"otlp,omitempty" yaml:"otlp,omitempty"`
}

func (c *DiagnosticsConfig) validate(stores map[string]bool) error {
	if c == nil || c.OTLP == nil {
		return nil
	}
	if err := c.OTLP.Validate(); err != nil {
		return fmt.Errorf("telemetry.diagnostics.otlp: %w", err)
	}
	if !c.OTLP.Enabled() {
		return nil
	}
	names := make([]string, 0, len(c.OTLP.Headers))
	for name := range c.OTLP.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateStoreRef(fmt.Sprintf("telemetry.diagnostics.otlp.headers[%q]", name), c.OTLP.Headers[name], stores); err != nil {
			return err
		}
	}
	return nil
}

// DiagnosticOTLP returns only the explicitly configured diagnostic destination.
// No environment lookup is allowed on this path, including generic OTEL vars.
func (c *Config) DiagnosticOTLP() OTLPConfig {
	if c == nil || c.Telemetry.Diagnostics == nil || c.Telemetry.Diagnostics.OTLP == nil {
		return OTLPConfig{}
	}
	return *c.Telemetry.Diagnostics.OTLP
}
