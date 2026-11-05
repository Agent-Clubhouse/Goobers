package main

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/goobers/goobers/internal/testshard"
)

// The snapshot is part of the runner binary, so every hosted shard uses the
// same measurements without fetching mutable artifacts or depending on cwd.
//
//go:embed weights.json
var measuredStressWeights []byte

type stressWeights struct {
	SchemaVersion int                         `json:"schemaVersion"`
	DefaultMillis int64                       `json:"defaultMillis"`
	Packages      map[string]map[string]int64 `json:"packages"`
}

func loadStressWeights(data []byte) (stressWeights, error) {
	var weights stressWeights
	if err := json.Unmarshal(data, &weights); err != nil {
		return weights, fmt.Errorf("decode stress weights: %w", err)
	}
	if weights.SchemaVersion != 1 || weights.DefaultMillis < 1 || weights.DefaultMillis > 60_000 || len(weights.Packages) == 0 {
		return weights, fmt.Errorf("invalid stress weight schema, fallback, or package inventory")
	}
	for pkg, tests := range weights.Packages {
		if len(tests) == 0 {
			return weights, fmt.Errorf("stress weights for %s are empty", pkg)
		}
		for name, millis := range tests {
			if !testNamePattern.MatchString(name) || millis < 0 || millis > stressTimeout.Milliseconds() {
				return weights, fmt.Errorf("invalid stress weight %s/%s: %dms", pkg, name, millis)
			}
		}
	}
	return weights, nil
}

func weightedStressTests(names []string, spec shardSpec) ([]string, error) {
	weights, err := loadStressWeights(measuredStressWeights)
	if err != nil {
		return nil, err
	}
	measured, ok := weights.Packages[spec.Package]
	if !ok {
		return nil, fmt.Errorf("no measured stress weights for sharded package %s", spec.Package)
	}
	groups := testshard.Partition(names, spec.Shards, func(name string) int64 {
		if millis, ok := measured[name]; ok {
			// Nonzero floor spreads rounded-zero tests across all shards.
			return max(millis, 10)
		}
		// New tests remain enrolled at a documented, bounded two-second
		// weight until another measured snapshot is reviewed.
		return weights.DefaultMillis
	})
	return groups[spec.Index], nil
}
