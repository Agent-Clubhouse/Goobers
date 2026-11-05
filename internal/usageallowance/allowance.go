// Package usageallowance carries trusted per-attempt token and cost ceilings
// from the deterministic retry walk to an isolated stage supervisor.
package usageallowance

import (
	"fmt"
	"math"
)

// Allowance is the remaining usage a single remote attempt may consume.
// Nil fields mean unlimited; a non-nil zero means the cumulative stage budget
// is exhausted and the supervisor must not start a harness.
type Allowance struct {
	MaxTokens  *int64   `json:"maxTokens,omitempty"`
	MaxCostUSD *float64 `json:"maxCostUSD,omitempty"`
}

// Validate rejects malformed trusted allowance data before it reaches a pod.
func (a Allowance) Validate() error {
	if a.MaxTokens != nil && *a.MaxTokens < 0 {
		return fmt.Errorf("usage allowance maxTokens must not be negative")
	}
	if a.MaxCostUSD != nil &&
		(*a.MaxCostUSD < 0 || math.IsNaN(*a.MaxCostUSD) || math.IsInf(*a.MaxCostUSD, 0)) {
		return fmt.Errorf("usage allowance maxCostUSD must be finite and non-negative")
	}
	return nil
}

// Clone returns an independent copy suitable for a wire contract.
func Clone(a *Allowance) *Allowance {
	if a == nil {
		return nil
	}
	out := &Allowance{}
	if a.MaxTokens != nil {
		value := *a.MaxTokens
		out.MaxTokens = &value
	}
	if a.MaxCostUSD != nil {
		value := *a.MaxCostUSD
		out.MaxCostUSD = &value
	}
	return out
}
