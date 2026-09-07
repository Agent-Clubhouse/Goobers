package instance

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/runnersolve"
)

// IsolationConfig declares the instance trust-root floor, never a stage override.
type IsolationConfig struct {
	Mandates []IsolationMandate `json:"mandates" yaml:"mandates"`
}

// IsolationMandate applies its complete restriction set to the selected class.
type IsolationMandate struct {
	Match        IsolationMatch      `json:"match" yaml:"match"`
	Restrictions []RunnerRestriction `json:"restrictions" yaml:"restrictions"`
}

// IsolationMatch deliberately has no wildcard or workflow-controlled selector.
type IsolationMatch struct {
	StageClass string `json:"stageClass" yaml:"stageClass"`
}

// HasIsolationMandates distinguishes the opt-in policy from legacy admission.
func (c *Config) HasIsolationMandates() bool {
	return c != nil && c.Isolation != nil && len(c.Isolation.Mandates) != 0
}

// PlacementInventory is the shared inventory construction for validation,
// boot admission and run-start pinning. Every caller carries the same floor.
func (c *Config) PlacementInventory(selfOS string) runnersolve.Inventory {
	inv := runnersolve.Inventory{Runners: c.PlacementRunners(selfOS)}
	if !c.HasIsolationMandates() {
		return inv
	}
	inv.ClassMandates = make(map[string][]string)
	for _, mandate := range c.Isolation.Mandates {
		for _, restriction := range mandate.Restrictions {
			inv.ClassMandates[mandate.Match.StageClass] = append(inv.ClassMandates[mandate.Match.StageClass], string(restriction))
		}
	}
	return inv
}

func (c *Config) validateIsolation() error {
	if c.Isolation == nil {
		return nil
	}
	if len(c.Isolation.Mandates) == 0 {
		return fmt.Errorf("isolation.mandates: at least one mandate is required; omit isolation to leave the floor disabled")
	}
	for i, mandate := range c.Isolation.Mandates {
		if mandate.Match.StageClass != "agentic" && mandate.Match.StageClass != "deterministic" {
			return fmt.Errorf("isolation.mandates[%d].match.stageClass: must be agentic or deterministic", i)
		}
		if len(mandate.Restrictions) == 0 {
			return fmt.Errorf("isolation.mandates[%d].restrictions: at least one effect is required", i)
		}
		seen := make(map[RunnerRestriction]bool)
		for _, restriction := range mandate.Restrictions {
			if !knownRunnerRestrictions[restriction] || seen[restriction] {
				return fmt.Errorf("isolation.mandates[%d].restrictions: unknown or duplicate effect %q (known: %s)", i, restriction, knownRunnerRestrictionNames())
			}
			seen[restriction] = true
		}
	}
	inv := c.PlacementInventory("")
	// Check the UNION for a class, not each mandate independently: two
	// individually satisfiable mandates can still have no common runner.
	for _, class := range []string{"agentic", "deterministic"} {
		if len(inv.ClassMandates[class]) == 0 {
			continue
		}
		result := runnersolve.Solve(inv, []runnersolve.StageRequirement{{Stage: "isolation.mandates[" + class + "]", StageClass: class}})
		if unsat := result.Unsatisfiable(); len(unsat) != 0 {
			return fmt.Errorf("isolation.mandates for %s (%s): %s", class, strings.Join(inv.ClassMandates[class], ", "), unsat[0].Unsat.Diagnostic)
		}
	}
	return nil
}
