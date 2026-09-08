package main

import (
	"errors"
	"fmt"
	"strconv"
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
	if raw := providerInput("maxItems", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return p, fmt.Errorf("invalid maxItems %q (want a positive integer)", raw)
		}
		p.maxItems = n
	}
	p.curation = mode == backlogQueryModeResweep || (mode == backlogQueryModeClaim && providerInput("curation", "false") == "true")
	var err error
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
	if p.resweepEnabled && !p.curation {
		return p, errors.New("re-sweep inputs are only valid for backlog-curation --claim runs or --resweep")
	}
	if mode == backlogQueryModeResweep && !p.resweepEnabled {
		return p, errors.New("--resweep requires a bounded resweepMaxItems input")
	}
	return p, nil
}
