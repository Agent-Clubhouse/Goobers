package instance

// placement.go converts the resolved runner inventory into the shared
// constraint solver's view (internal/runnersolve — the one implementation
// behind all three admission checkpoints, dsl-3.0.md §5 / open point 8).

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/goobers/goobers/internal/runnersolve"
)

// PlacementRunners returns the solver view of this instance's resolved
// runner inventory (ResolvedRunners: the declared runners: list, or the
// implicit self entry synthesized from the legacy singular runner: block).
//
// selfOS is the operating system substituted for a self entry that declares
// no provides.os — the daemon host's OS is a process fact, not a trusted
// claim, so callers pass runnersolve.HostOS() (`goobers validate` therefore
// reads the validating host's OS; for local modes the validating host is the
// executing host, and on an inventory-less instance every solve finding is
// advisory anyway). A self entry that DOES declare provides.os keeps its
// declaration. Non-self entries claim only what they declare (decision
// record D10: trusted claims; explicit-complete — an entry with no
// provides.os satisfies no OS requirement).
func (c *Config) PlacementRunners(selfOS string) []runnersolve.Runner {
	resolved := c.ResolvedRunners()
	runners := make([]runnersolve.Runner, 0, len(resolved))
	for _, entry := range resolved {
		runner := runnersolve.Runner{
			Name:         entry.Name,
			Self:         entry.Host == RunnerHostSelfName,
			OS:           string(entry.Provides.OS),
			CPU:          parsedCeiling(entry.Provides.CPU),
			Memory:       parsedCeiling(entry.Provides.Memory),
			Disk:         parsedCeiling(entry.Provides.Disk),
			Capabilities: entry.Provides.Capabilities,
		}
		for _, restriction := range entry.Restrictions {
			runner.Restrictions = append(runner.Restrictions, string(restriction))
		}
		if runner.Self && runner.OS == "" {
			runner.OS = selfOS
		}
		runners = append(runners, runner)
	}
	return runners
}

// parsedCeiling parses a declared provides quantity. Well-formedness is
// enforced at config load (validateRunners), so a spelling that fails to
// parse here can only mean the value was never validated — treat it as
// undeclared rather than inventing a ceiling.
func parsedCeiling(value string) *resource.Quantity {
	if value == "" {
		return nil
	}
	parsed, err := resource.ParseQuantity(value)
	if err != nil {
		return nil
	}
	return &parsed
}
