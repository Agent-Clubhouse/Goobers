package instance

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DiagnosticsConfig controls diagnostic export independently of run telemetry.
// Its collector is explicit: neither the journal collector nor ambient OTLP
// settings can opt an operator into exporting instance identity.
type DiagnosticsConfig struct {
	Organization      string            `json:"organization,omitempty" yaml:"organization,omitempty"`
	Environment       string            `json:"environment,omitempty" yaml:"environment,omitempty"`
	OwnerRef          string            `json:"ownerRef,omitempty" yaml:"ownerRef,omitempty"`
	GaggleOwners      map[string]string `json:"gaggleOwners,omitempty" yaml:"gaggleOwners,omitempty"`
	HeartbeatInterval string            `json:"heartbeatInterval,omitempty" yaml:"heartbeatInterval,omitempty"`
	ProgressTimeout   string            `json:"progressTimeout,omitempty" yaml:"progressTimeout,omitempty"`
	OTLP              *OTLPConfig       `json:"otlp,omitempty" yaml:"otlp,omitempty"`
}

func (c *DiagnosticsConfig) validate(stores map[string]bool) error {
	if c == nil {
		return nil
	}
	if err := c.validateFleet(); err != nil {
		return err
	}
	if c.OTLP == nil {
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

// HeartbeatPeriod returns the independent fleet observation interval.
func (c *DiagnosticsConfig) HeartbeatPeriod() time.Duration {
	if c == nil || c.HeartbeatInterval == "" {
		return 30 * time.Second
	}
	value, _ := time.ParseDuration(c.HeartbeatInterval)
	return value
}

// ProgressPeriod is the minimum time without useful progress before a stall
// can be inferred from eligible work. Known stage deadlines take precedence.
func (c *DiagnosticsConfig) ProgressPeriod() time.Duration {
	if c == nil || c.ProgressTimeout == "" {
		return 30 * time.Minute
	}
	value, _ := time.ParseDuration(c.ProgressTimeout)
	return value
}

func (c *DiagnosticsConfig) validateFleet() error {
	for _, field := range []struct{ name, value string }{
		{"organization", c.Organization}, {"environment", c.Environment}, {"ownerRef", c.OwnerRef},
	} {
		if !validDiagnosticReference(field.value) {
			return fmt.Errorf("telemetry.diagnostics.%s must be at most 256 bytes without control characters or surrounding whitespace", field.name)
		}
	}
	if len(c.GaggleOwners) > 1000 {
		return fmt.Errorf("telemetry.diagnostics.gaggleOwners exceeds 1000 mappings")
	}
	for name, owner := range c.GaggleOwners {
		if name == "" || strings.ContainsAny(name, "/\\") || !validDiagnosticReference(name) || owner == "" || !validDiagnosticReference(owner) {
			return fmt.Errorf("telemetry.diagnostics.gaggleOwners requires bounded gaggle names and nonempty owner references")
		}
	}
	for _, field := range []struct {
		name, value      string
		minimum, maximum time.Duration
	}{
		{"heartbeatInterval", c.HeartbeatInterval, 10 * time.Second, time.Hour},
		{"progressTimeout", c.ProgressTimeout, time.Minute, 7 * 24 * time.Hour},
	} {
		if field.value == "" {
			continue
		}
		value, err := time.ParseDuration(field.value)
		if err != nil || value < field.minimum || value > field.maximum {
			return fmt.Errorf("telemetry.diagnostics.%s must be a duration between %s and %s", field.name, field.minimum, field.maximum)
		}
	}
	return nil
}

func validDiagnosticReference(value string) bool {
	if len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
