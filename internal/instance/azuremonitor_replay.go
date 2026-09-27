package instance

import (
	"fmt"
	"time"
)

const (
	// DefaultAzureMonitorReplayMaxAge keeps enough outage history for the
	// tenant-telemetry v1 target without requiring extra configuration.
	DefaultAzureMonitorReplayMaxAge = 72 * time.Hour
	// DefaultAzureMonitorReplayMaxBytes bounds one instance's direct-export
	// spool. Operators with higher volume can raise it explicitly.
	DefaultAzureMonitorReplayMaxBytes int64 = 512 << 20
)

// AzureMonitorReplayConfig controls the bounded per-instance disk spool.
// Replay defaults on whenever the direct Azure Monitor destination is enabled.
type AzureMonitorReplayConfig struct {
	Enabled  *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	MaxAge   string `json:"maxAge,omitempty" yaml:"maxAge,omitempty"`
	MaxBytes int64  `json:"maxBytes,omitempty" yaml:"maxBytes,omitempty"`
}

// EnabledEffective reports whether durable replay is active. Nil defaults on.
func (c *AzureMonitorReplayConfig) EnabledEffective() bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

// MaxAgeDuration returns the configured bound or its three-day default.
func (c *AzureMonitorReplayConfig) MaxAgeDuration() time.Duration {
	if c == nil || c.MaxAge == "" {
		return DefaultAzureMonitorReplayMaxAge
	}
	value, _ := time.ParseDuration(c.MaxAge)
	return value
}

// MaxBytesEffective returns the configured byte bound or its default.
func (c *AzureMonitorReplayConfig) MaxBytesEffective() int64 {
	if c == nil || c.MaxBytes == 0 {
		return DefaultAzureMonitorReplayMaxBytes
	}
	return c.MaxBytes
}

func (c *AzureMonitorReplayConfig) validate() error {
	if c == nil {
		return nil
	}
	if c.MaxAge != "" {
		value, err := time.ParseDuration(c.MaxAge)
		if err != nil || value < time.Hour || value > 30*24*time.Hour {
			return fmt.Errorf("telemetry.azureMonitor.replay.maxAge must be a duration between 1h and 720h")
		}
	}
	if c.MaxBytes != 0 && (c.MaxBytes < 1<<20 || c.MaxBytes > 10<<30) {
		return fmt.Errorf("telemetry.azureMonitor.replay.maxBytes must be between 1048576 and 10737418240 bytes")
	}
	return nil
}
