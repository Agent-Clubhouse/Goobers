package main

import (
	"errors"
	"fmt"
)

type backlogQueryPolicies struct {
	maxItems       int
	curation       bool
	staleness      backlogStalenessPolicy
	resweep        backlogResweepPolicy
	resweepEnabled bool
}

func readBacklogQueryPolicies(mode backlogQueryMode) (backlogQueryPolicies, error) {
	p := backlogQueryPolicies{maxItems: 1}
	maxItems, err := parseIntInput(
		providerInput("maxItems", "1"),
		func(value int) bool { return value >= 1 },
		func(raw string, _ error) string {
			return fmt.Sprintf("invalid maxItems %q (want a positive integer)", raw)
		},
	)
	if err != nil {
		return p, err
	}
	p.maxItems = maxItems
	p.curation = mode == backlogQueryModeResweep || (mode == backlogQueryModeClaim && providerInput("curation", "false") == "true")
	// Claim output carries staleness evidence even when a separate stage
	// already reconciled metadata. Skipping that mutation pass must not turn
	// the configured threshold into the zero-value "everything is stale".
	if p.curation || mode == backlogQueryModeReconcile {
		p.staleness, err = readBacklogStalenessPolicy()
		if err != nil {
			return p, err
		}
	}
	p.resweep, p.resweepEnabled, err = readBacklogResweepPolicy(p.maxItems)
	if err != nil {
		return p, err
	}
	if mode != backlogQueryModeResweep && (p.resweepEnabled || providerInput("resweepReadyLabel", "") != "") {
		return p, errors.New("inline re-sweep is retired; move re-sweep inputs to a separate scheduled workflow using backlog-query --claim --resweep")
	}
	if mode == backlogQueryModeResweep && !p.resweepEnabled {
		return p, errors.New("--resweep requires a bounded resweepMaxItems input")
	}
	return p, nil
}
