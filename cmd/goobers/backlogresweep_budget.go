package main

import (
	"fmt"
	"strconv"
	"strings"
)

// defaultResweepDependencyMaxItems is the dependency-recheck lane's per-run
// budget when resweepDependencyMaxItems is unset (#4884). A daily sweep over
// a parked population of N then revisits each item within about N/25 days,
// rather than N/resweepMaxItems days behind the ready-drift lane.
const defaultResweepDependencyMaxItems = 25

// readResweepDependencyMaxItems reads the dependency-recheck lane's own
// budget (#4884). It is deliberately NOT bounded by maxItems: it caps how many
// parked items are rechecked, and only those whose native blockers have all
// closed go on to take a batch slot (bounded there by the remaining batch
// capacity). The scan ceiling bounds it because one listing window never
// yields more candidates than that.
func readResweepDependencyMaxItems() (int, error) {
	raw := strings.TrimSpace(providerInput("resweepDependencyMaxItems", ""))
	if raw == "" {
		return defaultResweepDependencyMaxItems, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > backlogScanCeiling {
		return 0, fmt.Errorf(
			"invalid resweepDependencyMaxItems %q (want an integer from 1 through %d)",
			raw,
			backlogScanCeiling,
		)
	}
	return n, nil
}
