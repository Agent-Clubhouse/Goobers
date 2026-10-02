package instance

import (
	"fmt"
	"sync/atomic"
)

// PlacementConfig controls where workflow work may execute. It does not govern
// daemon-internal scheduling, journaling, claim cleanup or reconciliation.
type PlacementConfig struct {
	SelfExecution string `json:"selfExecution,omitempty" yaml:"selfExecution,omitempty"`
}

// SelfExecutionPolicy resolves the backwards-compatible local default.
func (c *Config) SelfExecutionPolicy() string {
	if c == nil || c.Placement == nil || c.Placement.SelfExecution == "" {
		return "allow"
	}
	return c.Placement.SelfExecution
}

// SelfExecutionDenied excludes the daemon host from workflow execution.
func (c *Config) SelfExecutionDenied() bool { return c.SelfExecutionPolicy() == "deny" }

func (c *Config) validatePlacement() error {
	switch c.SelfExecutionPolicy() {
	case "allow", "deny":
		return nil
	default:
		return fmt.Errorf("placement.selfExecution: must be allow or deny, got %q", c.Placement.SelfExecution)
	}
}

// SelfExecutionStats reports process-lifetime observations for this config.
type SelfExecutionStats struct {
	Policy     string `json:"policy"`
	Observed   bool   `json:"observed"`
	Placements uint64 `json:"placements"`
	Refusals   uint64 `json:"refusals"`
}

type selfExecutionCounters struct {
	placements atomic.Uint64
	refusals   atomic.Uint64
}

// ObserveSelfExecution records a refusal before execution, or an actual local placement.
func (c *Config) ObserveSelfExecution(refused bool) {
	if c.selfExecution == nil {
		return
	}
	if refused {
		c.selfExecution.refusals.Add(1)
	} else {
		c.selfExecution.placements.Add(1)
	}
}

// SelfExecutionStats exposes both zero counts and the effective default policy.
func (c *Config) SelfExecutionStats() SelfExecutionStats {
	stats := SelfExecutionStats{Policy: c.SelfExecutionPolicy()}
	if c != nil && c.selfExecution != nil {
		stats.Observed = true
		stats.Placements = c.selfExecution.placements.Load()
		stats.Refusals = c.selfExecution.refusals.Load()
	}
	return stats
}

// StartSelfExecutionAccounting initializes live accounting during composition,
// before concurrent execution begins. Offline config readers remain unobserved.
func (c *Config) StartSelfExecutionAccounting() {
	if c.selfExecution == nil {
		c.selfExecution = &selfExecutionCounters{}
	}
}
