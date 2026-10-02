package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/readservice"
)

func isolationMandateStatusLines(status readservice.SchedulerStatus) string {
	var out strings.Builder
	if policy := status.SelfExecution; policy.Policy != "" {
		fmt.Fprintf(&out, "Self execution: %s (self placements: %d; refusals: %d; observed: %t)\n", policy.Policy, policy.Placements, policy.Refusals, policy.Observed)
	}
	for _, class := range []string{"agentic", "deterministic"} {
		if effects := status.IsolationMandates[class]; len(effects) > 0 {
			fmt.Fprintf(&out, "Isolation mandate (%s): %s\n", class, strings.Join(effects, ", "))
		}
	}
	for _, gaggle := range slices.Sorted(maps.Keys(status.StageServiceAccounts)) {
		fmt.Fprintf(&out, "Stage ServiceAccount (%s): %s\n", gaggle, status.StageServiceAccounts[gaggle])
	}
	return out.String()
}
